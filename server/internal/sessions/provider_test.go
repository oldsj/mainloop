package sessions_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

const (
	codexAgentName = "mainloop-main-codex"
	reportID       = "6b1f4c52-0000-4000-8000-000000000002"
)

// providerEnv is an env with the provider registry (so the Codex main Agent exists in the fake).
func providerEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.withProviders()
	return e
}

func (e *env) selectProvider(provider string, restart bool) (sessions.MainProviderState, error) {
	e.t.Helper()
	return e.svc.SelectMainProvider(context.Background(), provider, restart)
}

func (e *env) mustSelect(provider string, restart bool) sessions.MainProviderState {
	e.t.Helper()
	st, err := e.selectProvider(provider, restart)
	if err != nil {
		e.t.Fatalf("select %s: %v", provider, err)
	}
	return st
}

func (e *env) providerState() sessions.MainProviderState {
	e.t.Helper()
	st, err := e.svc.MainProviderState(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *env) mainID() string {
	e.t.Helper()
	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return m.SessionID
}

// warm starts the main on its current provider and completes one message.
func (e *env) warm() (stop func(), ksid string) {
	e.t.Helper()
	stop = e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	return stop, e.bindingKsid(e.mainID())
}

func (e *env) historyRows() []dbq.MainProviderHistory {
	e.t.Helper()
	rows, err := dbq.New(e.pool).ListMainProviderHistory(context.Background(), e.mainID())
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func (e *env) ksState(id string) kagentapi.RuntimeState {
	e.t.Helper()
	s, ok := e.srv.Session(id)
	if !ok {
		e.t.Fatalf("kagent has no Session %s", id)
	}
	return s.GetState()
}

func (e *env) createsOn(name string) (n int) {
	for _, r := range e.srv.CreateRequests() {
		if r.GetAgent().GetName() == name {
			n++
		}
	}
	return n
}

// refused asserts the switch was refused with ErrConflict and that nothing durable moved.
func (e *env) refused(what string, before dbq.GetMainBindingRow) {
	e.t.Helper()
	_, err := e.selectProvider("codex", false)
	if !errors.Is(err, sessions.ErrConflict) {
		e.t.Fatalf("%s: err = %v, want ErrConflict", what, err)
	}
	now := e.bindingRow()
	if now.Kind != before.Kind || now.MainPending || deref(now.KagentSessionID) != deref(before.KagentSessionID) ||
		deref(now.KagentRequestID) != deref(before.KagentRequestID) {
		e.t.Fatalf("%s: the binding changed: %+v", what, now)
	}
	if h := e.historyRows(); len(h) != 0 {
		e.t.Fatalf("%s: history = %+v", what, h)
	}
	if n := e.createsOn(codexAgentName); n != 0 {
		e.t.Fatalf("%s: %d creates on the Codex Agent", what, n)
	}
	if before.KagentSessionID != nil && e.ksState(*before.KagentSessionID) != kagentapi.RuntimeState_RUNTIME_STATE_READY {
		e.t.Fatalf("%s: the old Session is %s", what, e.ksState(*before.KagentSessionID))
	}
}

// ---- first use ----------------------------------------------------------------------------------

func TestFirstUseStartsOnTheConfiguredProvider(t *testing.T) {
	for _, tc := range []struct {
		name, setting, kind, agent string
	}{
		{"unset defaults to claude", "", "claude", "mainloop-main"},
		{"claude", "claude", "claude", "mainloop-main"},
		{"codex", "codex", "codex", codexAgentName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := providerEnv(t)
			e.svc.MainProvider = tc.setting
			st := e.providerState()
			if st.Desired != tc.kind || st.Status != "not_started" || st.Effective != nil || st.KagentSessionID != nil {
				t.Fatalf("before the first message: %+v", st)
			}
			e.run()
			e.waitState(e.send("hello"), dbstate.DeliveryCompleted)
			reqs := e.srv.CreateRequests()
			if len(reqs) != 1 || reqs[0].GetAgent().GetName() != tc.agent {
				t.Fatalf("creates = %v, want one on %s", reqs, tc.agent)
			}
			st = e.providerState()
			if st.Desired != tc.kind || st.Status != "ready" || st.Effective == nil || *st.Effective != tc.kind ||
				st.KagentSessionID == nil || len(st.History) != 0 {
				t.Fatalf("after the first message: %+v", st)
			}
		})
	}
}

func TestFirstUseWithoutARegistryUsesTheConfiguredAgents(t *testing.T) {
	e := newEnv(t)
	e.svc.MainCodexAgent = kagent.AgentRef{Namespace: agent.Namespace, Name: codexAgentName}
	e.svc.MainProvider = "codex"
	e.srv.AddAgent(agent.Namespace, codexAgentName)
	e.run()
	e.waitState(e.send("hello"), dbstate.DeliveryCompleted)
	if e.createsOn(codexAgentName) != 1 {
		t.Fatalf("creates = %v", e.srv.CreateRequests())
	}
}

// ---- the idle rule ------------------------------------------------------------------------------

func TestASwitchIsRefusedUnlessTheMainIsIdle(t *testing.T) {
	t.Run("recorded", func(t *testing.T) {
		e := providerEnv(t)
		e.send("waiting") // no delivery loop: the message stays recorded
		e.refused("recorded", e.bindingRow())
	})
	for _, state := range []dbstate.DeliveryState{dbstate.DeliverySending, dbstate.DeliveryDelivered, dbstate.DeliveryUncertain} {
		t.Run(string(state), func(t *testing.T) {
			e := providerEnv(t)
			stop, _ := e.warm()
			stop()
			e.leftBehind(state, "t-left", "in flight")
			e.refused(string(state), e.bindingRow())
		})
	}
	t.Run("an open native turn with no ledger row", func(t *testing.T) {
		e := providerEnv(t)
		stop, ksid := e.warm()
		stop()
		e.srv.AddTask(runningTask("t-open", ksid, "m-elsewhere", "half"))
		e.refused("native turn", e.bindingRow())
	})
	t.Run("an unsettled lifecycle operation", func(t *testing.T) {
		e := providerEnv(t)
		stop, _ := e.warm()
		stop()
		e.srv.TamperSession = func(proc string, s *kagentapi.Session) {
			if proc == kagent.ProcGetSession {
				s.Operation = kagentapi.RuntimeOperation_RUNTIME_OPERATION_SUSPEND
			}
		}
		e.refused("operation in flight", e.bindingRow())
	})
	t.Run("an unconfirmed stop", func(t *testing.T) {
		e := providerEnv(t)
		stop, _ := e.warm()
		stop()
		if _, err := e.pool.Exec(context.Background(), `UPDATE sessions SET agent_cancel_outcome = 'unknown' WHERE id = $1`, e.mainID()); err != nil {
			t.Fatal(err)
		}
		e.refused("unconfirmed stop", e.bindingRow())
	})
	t.Run("an unresolved create", func(t *testing.T) {
		e := providerEnv(t)
		e.run()
		id := e.mainID()
		// The create was requested and its reply never came back: no Session id is recorded.
		if _, err := e.pool.Exec(context.Background(),
			`UPDATE agent_bindings SET kagent_create_request_id = 'lost-reply' WHERE session_id = $1`, id); err != nil {
			t.Fatal(err)
		}
		e.refused("unresolved create", e.bindingRow())
	})
	t.Run("kagent cannot confirm", func(t *testing.T) {
		e := providerEnv(t)
		stop, _ := e.warm()
		stop()
		code := connect.CodeUnavailable
		e.srv.FailNext(kagent.ProcGetSession, kagenttest.Fault{GRPC: &code})
		e.srv.FailNext(kagent.ProcGetSession, kagenttest.Fault{GRPC: &code})
		e.srv.FailNext(kagent.ProcGetSession, kagenttest.Fault{GRPC: &code})
		e.refused("kagent unavailable", e.bindingRow())
	})
}

// ---- a successful switch ------------------------------------------------------------------------

func TestASwitchSuspendsTheOldSessionRevokesItsCredentialAndKeepsTheConversation(t *testing.T) {
	e := providerEnv(t)
	a := e.withCredentials()
	cleanup := runCredentialCleanup(t, e, a)
	defer cleanup()
	stop, claudeKsid := e.warm()
	main := e.mainID()
	messagesBefore := len(e.messages())
	oldCreds := e.credentialRows(main)
	if len(oldCreds) != 1 || oldCreds[0].State != dbstate.CredentialPublished {
		t.Fatalf("credentials before = %+v", oldCreds)
	}
	oldRequest := deref(e.bindingRow().KagentRequestID)

	st := e.mustSelect("codex", false)
	if st.Desired != "codex" || st.Status != "ready" || st.Effective == nil || *st.Effective != "codex" || st.Recoverable || st.Error != nil {
		t.Fatalf("state = %+v", st)
	}
	codexKsid := deref(st.KagentSessionID)
	if codexKsid == "" || codexKsid == claudeKsid {
		t.Fatalf("Codex Session %q (Claude's %q)", codexKsid, claudeKsid)
	}
	if got := e.ksState(claudeKsid); got != kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED {
		t.Fatalf("old Session is %s, want suspended", got)
	}
	if len(st.History) != 1 || st.History[0].Provider != "claude" || deref(st.History[0].KagentSessionID) != claudeKsid {
		t.Fatalf("history = %+v", st.History)
	}
	rows := e.historyRows()
	if len(rows) != 1 || rows[0].Abandoned || deref(rows[0].RequestID) != oldRequest || rows[0].Turns != 1 {
		t.Fatalf("history rows = %+v", rows)
	}
	// The old credential is revoked, the new Session carries a new one.
	var revoked, live int
	for _, c := range e.credentialRows(main) {
		switch c.State {
		case dbstate.CredentialPublished:
			live++
		case dbstate.CredentialRevoked, dbstate.CredentialDeleted:
			revoked++
		}
	}
	if revoked != 1 || live != 1 {
		t.Fatalf("credentials: %d revoked, %d live: %+v", revoked, live, e.credentialRows(main))
	}
	reqs := e.srv.CreateRequests()
	last := reqs[len(reqs)-1]
	if last.GetAgent().GetName() != codexAgentName || len(last.GetCredentials()) != 1 || last.GetRequestId() == oldRequest {
		t.Fatalf("Codex create = %v", last)
	}
	// The product session and its conversation are the same; nothing was replayed.
	if e.mainID() != main || len(e.messages()) != messagesBefore {
		t.Fatalf("main %s -> %s, messages %d -> %d", main, e.mainID(), messagesBefore, len(e.messages()))
	}
	sends := e.sends()
	if st.Status == "ready" && e.bindingRow().Turns != 0 {
		t.Fatalf("turns = %d on the new identity", e.bindingRow().Turns)
	}
	// The next message goes to the Codex Session; the old one is left alone.
	_ = stop
	e.waitState(e.send("two"), dbstate.DeliveryCompleted)
	if e.sends() != sends+1 || e.bindingKsid(main) != codexKsid {
		t.Fatalf("sends %d -> %d, bound %s", sends, e.sends(), e.bindingKsid(main))
	}
	if got := e.ksState(claudeKsid); got != kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED {
		t.Fatalf("old Session is %s after the next message", got)
	}
	// The retired Session is still Mainloop's: the orphan sweep leaves it alone.
	if rep := e.orphans(e.svc.Kagent.(*kagent.Client)); len(rep.Deleted) != 0 {
		t.Fatalf("orphan sweep deleted %v", rep.Deleted)
	}
}

func TestASwitchBeforeTheFirstMessageNeedsNoOldSession(t *testing.T) {
	e := providerEnv(t)
	st := e.mustSelect("codex", false)
	if st.Status != "ready" || st.Desired != "codex" {
		t.Fatalf("state = %+v", st)
	}
	// A main that never started has no native identity to keep.
	if h := e.historyRows(); len(h) != 0 {
		t.Fatalf("history = %+v, want none", h)
	}
	if e.createsOn(codexAgentName) != 1 || e.createsOn("mainloop-main") != 0 {
		t.Fatalf("creates = %v", e.srv.CreateRequests())
	}
}

func TestSwitchingBackStartsAFreshClaudeSession(t *testing.T) {
	e := providerEnv(t)
	stop, claude1 := e.warm()
	stop()
	e.mustSelect("codex", false)
	st := e.mustSelect("claude", false)
	if st.Status != "ready" || st.Desired != "claude" || len(st.History) != 2 {
		t.Fatalf("state = %+v", st)
	}
	if k := deref(st.KagentSessionID); k == "" || k == claude1 {
		t.Fatalf("Claude Session %q after switching back (was %q)", k, claude1)
	}
}

// ---- repeats ------------------------------------------------------------------------------------

func TestRepeatingTheSamePUTChangesNothing(t *testing.T) {
	e := providerEnv(t)
	stop, _ := e.warm()
	stop()
	first := e.mustSelect("codex", false)
	creates := e.srv.Count(kagent.ProcCreateSession)
	second := e.mustSelect("codex", false)
	if second.Status != "ready" || deref(second.KagentSessionID) != deref(first.KagentSessionID) ||
		len(second.History) != 1 || e.srv.Count(kagent.ProcCreateSession) != creates {
		t.Fatalf("second = %+v (creates %d -> %d)", second, creates, e.srv.Count(kagent.ProcCreateSession))
	}
	// Selecting the current provider before any switch is also a no-op.
	e2 := providerEnv(t)
	if st := e2.mustSelect("claude", false); st.Status != "not_started" || len(st.History) != 0 || e2.srv.Count(kagent.ProcCreateSession) != 0 {
		t.Fatalf("no-op state = %+v", st)
	}
}

func TestAnInvalidProviderIsRefused(t *testing.T) {
	e := providerEnv(t)
	if _, err := e.selectProvider("gemini", false); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	e.svc.Kagent = nil
	if _, err := e.selectProvider("codex", false); !errors.Is(err, sessions.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestBothProvidersCannotShareOneAgent(t *testing.T) {
	e := newEnv(t)
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "KAGENT_MAIN_CODEX_AGENT": "mainloop-main"})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Providers = reg
	if _, err := e.selectProvider("codex", false); !errors.Is(err, sessions.ErrInvalid) || !strings.Contains(err.Error(), "agent_conflict") {
		t.Fatalf("err = %v, want an agent conflict", err)
	}
}

// ---- crash and recovery -------------------------------------------------------------------------

var errCrash = errors.New("simulated crash")

// crashAfterIntent leaves the main with a committed switch intent and no create sent.
func crashAfterIntent(t *testing.T, e *env, provider string) (before dbq.GetMainBindingRow) {
	t.Helper()
	e.svc.AfterIntent = func(context.Context) error { return errCrash }
	if _, err := e.selectProvider(provider, false); !errors.Is(err, errCrash) {
		t.Fatalf("err = %v, want the crash", err)
	}
	e.svc.AfterIntent = nil
	return e.bindingRow()
}

func TestACrashAfterTheIntentCommitsRecoversTheSameContract(t *testing.T) {
	e := providerEnv(t)
	stop, claudeKsid := e.warm()
	stop()
	e.run()
	row := crashAfterIntent(t, e, "codex")
	if !row.MainPending || row.Kind != "codex" || row.KagentSessionID != nil || row.KagentCreateRequestID == nil ||
		deref(row.KagentAgentName) != codexAgentName {
		t.Fatalf("intent = %+v", row)
	}
	frozen := deref(row.KagentCreateRequestID)
	if e.createsOn(codexAgentName) != 0 {
		t.Fatal("a create was sent before the intent was repeated")
	}
	if got := e.ksState(claudeKsid); got != kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED {
		t.Fatalf("old Session is %s", got)
	}
	st := e.providerState()
	if st.Status != "pending" || st.Desired != "codex" || st.Effective != nil || st.Recoverable {
		t.Fatalf("state after the crash = %+v", st)
	}

	st = e.mustSelect("codex", false)
	if st.Status != "ready" || st.Effective == nil || *st.Effective != "codex" {
		t.Fatalf("state after the repeat = %+v", st)
	}
	reqs := e.srv.CreateRequests()
	if last := reqs[len(reqs)-1]; last.GetRequestId() != frozen || last.GetAgent().GetName() != codexAgentName {
		t.Fatalf("create = %v, want request %s on %s", last, frozen, codexAgentName)
	}
	if e.createsOn(codexAgentName) != 1 || len(e.historyRows()) != 1 {
		t.Fatalf("creates on Codex = %d, history = %d", e.createsOn(codexAgentName), len(e.historyRows()))
	}
}

func TestADifferentProviderCannotReplaceAnUnresolvedSelection(t *testing.T) {
	e := providerEnv(t)
	crashAfterIntent(t, e, "codex")
	before := e.bindingRow()
	if _, err := e.selectProvider("claude", false); !errors.Is(err, sessions.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	// restart_failed does not make a pending, not-yet-failed selection replaceable either.
	if _, err := e.selectProvider("claude", true); !errors.Is(err, sessions.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	now := e.bindingRow()
	if now.Kind != "codex" || !now.MainPending || deref(now.KagentRequestID) != deref(before.KagentRequestID) || len(e.historyRows()) != 0 {
		t.Fatalf("binding = %+v", now)
	}
	// restart_failed on the same provider only recovers the same contract while it is not recoverable.
	st := e.mustSelect("codex", true)
	if st.Status != "ready" || e.createsOn(codexAgentName) != 1 || len(e.historyRows()) != 0 {
		t.Fatalf("state = %+v, history %d", st, len(e.historyRows()))
	}
}

func TestChatIsRefusedWhilePendingAndAReportWaitsThenDrains(t *testing.T) {
	e := providerEnv(t)
	stop, _ := e.warm()
	stop()
	crashAfterIntent(t, e, "codex")
	main := e.mainID()
	messages := len(e.messages())

	if _, err := e.svc.Send(context.Background(), uuid.NewString(), "", "hello"); !errors.Is(err, sessions.ErrConflict) {
		t.Fatalf("chat err = %v, want ErrConflict", err)
	}
	// A child report goes in through SendTo under a stable message id, and is retried until it takes.
	if _, err := e.svc.SendTo(context.Background(), main, reportID, "[report from task]"); !errors.Is(err, sessions.ErrConflict) {
		t.Fatalf("report err = %v, want ErrConflict", err)
	}
	if len(e.messages()) != messages {
		t.Fatalf("messages %d -> %d: a refused send was recorded", messages, len(e.messages()))
	}
	if n := e.deliveriesFor(reportID); n != 0 {
		t.Fatalf("%d deliveries for the refused report", n)
	}

	e.run()
	e.mustSelect("codex", false)
	if _, err := e.svc.SendTo(context.Background(), main, reportID, "[report from task]"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	d := e.waitState(reportID, dbstate.DeliveryCompleted)
	if deref(d.KagentSessionID) != e.bindingKsid(main) {
		t.Fatalf("delivered to %s, want the Codex Session %s", deref(d.KagentSessionID), e.bindingKsid(main))
	}
	// A retry of the same message id after delivery is the same message.
	if _, err := e.svc.SendTo(context.Background(), main, reportID, "[report from task]"); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if e.deliveriesFor(reportID) != 1 {
		t.Fatal("the report was recorded twice")
	}
}

func (e *env) deliveriesFor(id string) (n int) {
	e.t.Helper()
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM deliveries WHERE id = $1`, id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestACatalogChangeDoesNotMoveAPinnedContract(t *testing.T) {
	e := providerEnv(t)
	crashAfterIntent(t, e, "codex")
	// The operator now names another Codex main Agent. The pending contract keeps the first one.
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "KAGENT_MAIN_CODEX_AGENT": "mainloop-main-codex-2"})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.AddAgent(agent.Namespace, "mainloop-main-codex-2")
	e.svc.Providers = reg
	e.svc.MainCodexAgent = kagent.AgentRef{Namespace: agent.Namespace, Name: "mainloop-main-codex-2"}

	st := e.mustSelect("codex", false)
	if st.Status != "ready" || e.createsOn(codexAgentName) != 1 || e.createsOn("mainloop-main-codex-2") != 0 {
		t.Fatalf("state %+v, creates %v", st, e.srv.CreateRequests())
	}
	// Later sends reach the Session on the pinned Agent, not the renamed one.
	e.run()
	e.waitState(e.send("hello"), dbstate.DeliveryCompleted)
	if e.sendsTo(codexAgentName) == 0 || e.sendsTo("mainloop-main-codex-2") != 0 {
		t.Fatalf("sends: pinned %d, renamed %d", e.sendsTo(codexAgentName), e.sendsTo("mainloop-main-codex-2"))
	}
}

func (e *env) sendsTo(agentName string) (n int) {
	for _, r := range e.srv.Requests() {
		if r.AgentPath == kagenttest.AgentPath(agent.Namespace, agentName) {
			n++
		}
	}
	return n
}

// ---- an absent or failed target -----------------------------------------------------------------

// pendingWithASession leaves a switch whose target Session exists but could not be made ready.
func pendingWithASession(t *testing.T, e *env) (ksid string) {
	t.Helper()
	code := connect.CodeInternal
	// The reads that follow the create fail: the Session is not confirmed ready, nor confirmed gone.
	e.svc.AfterIntent = func(context.Context) error {
		for range 2 {
			e.srv.FailNext(kagent.ProcGetSession, kagenttest.Fault{GRPC: &code})
		}
		return nil
	}
	st := e.mustSelect("codex", false)
	e.svc.AfterIntent = nil
	if st.Status != "failed" || st.Recoverable || st.Error == nil || st.KagentSessionID == nil {
		t.Fatalf("state = %+v, want a failed, not recoverable, pending target with a Session", st)
	}
	return *st.KagentSessionID
}

func TestAnAbsentTargetIsRecoverableAndRestartFailedAdmitsAFreshIdentity(t *testing.T) {
	e := providerEnv(t)
	stop, claudeKsid := e.warm()
	stop()
	target := pendingWithASession(t, e)
	first := e.bindingRow()
	if !first.MainPending || deref(first.MainError) == "" {
		t.Fatalf("binding = %+v", first)
	}
	// While the failure is not known to be final, another provider cannot take over.
	if _, err := e.selectProvider("claude", false); !errors.Is(err, sessions.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	// restart_failed does not admit a fresh identity either, until kagent shows it is gone.
	if st := e.mustSelect("codex", true); st.Status != "ready" || deref(st.KagentSessionID) != target {
		t.Fatalf("state = %+v: a live target was replaced", st)
	}

	// Start again: this time the target disappears behind Mainloop's back.
	e2 := providerEnv(t)
	stop2, _ := e2.warm()
	stop2()
	target2 := pendingWithASession(t, e2)
	if _, err := e2.other().DeleteSession(context.Background(), target2); err != nil {
		t.Fatal(err)
	}
	st := e2.mustSelect("codex", false)
	if st.Status != "failed" || !st.Recoverable || st.Error == nil {
		t.Fatalf("state = %+v, want failed and recoverable", st)
	}
	again := e2.bindingRow()
	if !again.MainRecoverable || !again.MainPending {
		t.Fatalf("recoverable is not durable: %+v", again)
	}
	creates := e2.srv.Count(kagent.ProcCreateSession)
	st = e2.mustSelect("codex", true)
	if st.Status != "ready" || st.Recoverable || deref(st.KagentSessionID) == target2 {
		t.Fatalf("state = %+v", st)
	}
	if e2.srv.Count(kagent.ProcCreateSession) != creates+1 || deref(e2.bindingRow().KagentRequestID) == deref(again.KagentRequestID) {
		t.Fatal("restart did not use a fresh identity")
	}
	h := e2.historyRows()
	if len(h) != 2 || !h[1].Abandoned || deref(h[1].KagentSessionID) != target2 || h[1].Provider != "codex" {
		t.Fatalf("history = %+v", h)
	}
	_ = claudeKsid
}

func TestARecoverableTargetCanBeReplacedByAnotherProvider(t *testing.T) {
	e := providerEnv(t)
	stop, _ := e.warm()
	stop()
	target := pendingWithASession(t, e)
	if _, err := e.other().DeleteSession(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if st := e.mustSelect("codex", false); !st.Recoverable {
		t.Fatalf("state = %+v", st)
	}
	st := e.mustSelect("claude", false)
	if st.Status != "ready" || st.Desired != "claude" || len(st.History) != 2 {
		t.Fatalf("state = %+v", st)
	}
}

func TestATargetCreateThatKagentDeletedIsRecoverable(t *testing.T) {
	e := providerEnv(t)
	crashAfterIntent(t, e, "codex")
	row := e.bindingRow()
	// The create landed and the Session was deleted before the reply was recorded: kagent keeps the
	// request id of a deleted Session and refuses a replay of it.
	made, err := e.other().CreateSession(context.Background(), &kagentapi.CreateSessionRequest{
		Agent:     &kagentapi.ResourceReference{Namespace: agent.Namespace, Name: codexAgentName},
		RequestId: deref(row.KagentRequestID), Name: "Mainloop main thread",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.other().DeleteSession(context.Background(), made.GetId()); err != nil {
		t.Fatal(err)
	}
	st := e.mustSelect("codex", false)
	if st.Status != "failed" || !st.Recoverable {
		t.Fatalf("state = %+v", st)
	}
	st = e.mustSelect("codex", true)
	if st.Status != "ready" || deref(st.KagentSessionID) == made.GetId() {
		t.Fatalf("state = %+v", st)
	}
}

func TestAFailedCreateIsRecordedBoundedAndNotRetried(t *testing.T) {
	e := providerEnv(t)
	code := connect.CodeUnavailable
	e.srv.FailNext(kagent.ProcCreateSession, kagenttest.Fault{GRPC: &code, Message: strings.Repeat("x", 2000) + " Bearer abcdefghijklmnop"})
	st := e.mustSelect("codex", false)
	if st.Status != "failed" || st.Recoverable || st.Error == nil || len(*st.Error) > 400 || strings.Contains(*st.Error, "abcdefghijklmnop") {
		t.Fatalf("state = %+v", st)
	}
	creates := e.srv.Count(kagent.ProcCreateSession)
	time.Sleep(200 * time.Millisecond)
	if e.srv.Count(kagent.ProcCreateSession) != creates {
		t.Fatal("a failed create was retried without a repeated PUT")
	}
	// A repeated PUT recovers the same contract.
	frozen := deref(e.bindingRow().KagentRequestID)
	st = e.mustSelect("codex", false)
	if st.Status != "ready" || deref(e.bindingRow().KagentRequestID) != frozen {
		t.Fatalf("state = %+v", st)
	}
}

// ---- the owner slot -----------------------------------------------------------------------------

func TestConcurrentFirstUseSharesOneMain(t *testing.T) {
	e := providerEnv(t)
	ids := e.ensureConcurrently(8)
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("ids = %v", ids)
		}
	}
	if n := e.liveMains(); n != 1 {
		t.Fatalf("%d live mains", n)
	}
}

func TestAMainThatLostCoordinationAuthorityIsReplacedOnceAndItsCredentialRevoked(t *testing.T) {
	e := providerEnv(t)
	a := e.withCredentials()
	ctx := context.Background()
	old := e.mainID()
	if _, err := a.Creds.Issue(ctx, old, dbstate.CredentialMCP); err != nil {
		t.Fatal(err)
	}
	// Reuse while the binding has active coordination authority.
	if again := e.mainID(); again != old {
		t.Fatalf("main changed from %s to %s", old, again)
	}
	if _, err := dbq.New(e.pool).SetBindingGrant(ctx, dbq.SetBindingGrantParams{SessionID: old, MCPGrantKind: dbstate.GrantWorkspace}); err != nil {
		t.Fatal(err)
	}
	ids := e.ensureConcurrently(8)
	for _, id := range ids {
		if id != ids[0] || id == old {
			t.Fatalf("ids = %v (old %s)", ids, old)
		}
	}
	if n := e.liveMains(); n != 1 {
		t.Fatalf("%d live mains", n)
	}
	var released bool
	if err := e.pool.QueryRow(ctx, `SELECT main_released_at IS NOT NULL FROM agent_bindings WHERE session_id = $1`, old).Scan(&released); err != nil || !released {
		t.Fatalf("old main released = %v, err %v", released, err)
	}
	for _, c := range e.credentialRows(old) {
		if c.State == dbstate.CredentialPublished || c.State == dbstate.CredentialPlanned {
			t.Fatalf("the replaced main's credential is %s", c.State)
		}
	}
}

func (e *env) ensureConcurrently(n int) []string {
	e.t.Helper()
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			m, err := e.svc.EnsureMain(context.Background())
			if err != nil {
				e.t.Error(err)
				return
			}
			ids[i] = m.SessionID
		})
	}
	wg.Wait()
	return ids
}

func (e *env) liveMains() (n int) {
	e.t.Helper()
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_bindings WHERE role = 'main' AND main_released_at IS NULL`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// ---- helpers ------------------------------------------------------------------------------------

// runCredentialCleanup runs the loop that deletes revoked credentials' Secrets. A switch does not
// wait for it: each native identity has its own Secret name.
func runCredentialCleanup(t *testing.T, e *env, a *authtest.Env) (stop func()) {
	t.Helper()
	r := reconcile.NewRunner(e.pool, quiet(), nil)
	a.Creds.Register(r)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = r.Run(ctx) })
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); wg.Wait() }) }
	t.Cleanup(stop)
	return stop
}

func TestAProviderStateIsOnTheMainThreadView(t *testing.T) {
	e := providerEnv(t)
	stop, _ := e.warm()
	stop()
	e.mustSelect("codex", false)
	resp, err := e.API().GetMainThread(context.Background(), api.GetMainThreadRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	info := resp.(api.GetMainThread200JSONResponse)
	p := info.Provider
	if p == nil || p.DesiredProvider != api.MainProviderCodex || p.Status != api.MainProviderInfoStatusReady ||
		p.EffectiveProvider == nil || *p.EffectiveProvider != api.MainProviderCodex || len(p.History) != 1 {
		t.Fatalf("provider = %+v", p)
	}
	if info.Native == nil || info.Native.AgentName != codexAgentName {
		t.Fatalf("native = %+v", info.Native)
	}
}
