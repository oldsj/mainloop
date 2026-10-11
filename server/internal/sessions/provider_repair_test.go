package sessions_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/sessions"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// codeOf is the stable `code` of a 409 as the HTTP layer writes it ("" when there is none).
func codeOf(t *testing.T, err error) string {
	t.Helper()
	rec := httptest.NewRecorder()
	apierr.Write(rec, err)
	if rec.Code != 409 {
		t.Fatalf("status = %d (%v), want 409", rec.Code, err)
	}
	var body struct {
		Detail string `json:"detail"`
		Code   string `json:"code"`
	}
	if jerr := json.Unmarshal(rec.Body.Bytes(), &body); jerr != nil || body.Detail == "" {
		t.Fatalf("body %q: %v", rec.Body.String(), jerr)
	}
	return body.Code
}

func putRequest(provider string, restart bool) api.PutMainProviderRequestObject {
	return api.PutMainProviderRequestObject{Body: &api.MainProviderSelection{
		Provider: api.MainProvider(provider), RestartFailed: &restart,
	}}
}

// putCode is the 409 code of a PUT /main-thread/provider the service refuses.
func (e *env) putCode(provider string, restart bool) string {
	e.t.Helper()
	_, err := e.API().PutMainProvider(context.Background(), putRequest(provider, restart))
	return codeOf(e.t, err)
}

func (e *env) liveCredentialCount(id string) (n int) {
	for _, c := range e.credentialRows(id) {
		if c.State == dbstate.CredentialPublished {
			n++
		}
	}
	return n
}

func (e *env) liveSessionsOn(name string) (n int) {
	e.t.Helper()
	all, err := e.other().ListSessions(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, s := range all {
		if s.GetAgent().GetName() == name && s.GetState() != kagentapi.RuntimeState_RUNTIME_STATE_DELETED {
			n++
		}
	}
	return n
}

// ---- the target Agent preflight -----------------------------------------------------------------

func TestAMissingOrUnpreparedTargetAgentIsRefusedBeforeAnythingChanges(t *testing.T) {
	for name, breakAgent := range map[string]func(e *env){
		"missing":      func(e *env) { e.srv.RemoveAgent(agent.Namespace, codexAgentName) },
		"not prepared": func(e *env) { e.srv.AddAgentNotReady(agent.Namespace, codexAgentName) },
	} {
		t.Run(name, func(t *testing.T) {
			e := providerEnv(t)
			e.withCredentials()
			stop, ksid := e.warm()
			stop()
			main := e.mainID()
			before := e.bindingRow()
			breakAgent(e)
			if code := e.putCode("codex", false); code != "agent_unavailable" {
				t.Fatalf("code = %q, want agent_unavailable", code)
			}
			// restart_failed does not skip the check.
			if code := e.putCode("codex", true); code != "agent_unavailable" {
				t.Fatalf("restart code = %q", code)
			}
			e.refused(name, before) // binding, history, creates and the old Session are unchanged
			if e.ksState(ksid) != kagentapi.RuntimeState_RUNTIME_STATE_READY {
				t.Fatalf("the old Session is %s", e.ksState(ksid))
			}
			if e.liveCredentialCount(main) != 1 {
				t.Fatalf("credentials = %+v: the old one must stay valid", e.credentialRows(main))
			}
			// The main still works.
			e.run()
			e.waitState(e.send("still here"), dbstate.DeliveryCompleted)
		})
	}
}

// The Agent is deleted after the preflight and before the create. kagent refuses the create before
// it reserves the request id (see createRejectedBeforeReservation), so nothing was created and the
// failure is recoverable: the owner can switch back.
func TestAnAgentRemovedBetweenThePreflightAndTheCreateEndsRecoverable(t *testing.T) {
	e := providerEnv(t)
	stop, _ := e.warm()
	stop()
	e.svc.AfterIntent = func(context.Context) error {
		e.srv.RemoveAgent(agent.Namespace, codexAgentName)
		return nil
	}
	st := e.mustSelect("codex", false)
	e.svc.AfterIntent = nil
	if st.Status != "failed" || !st.Recoverable || st.Error == nil || st.KagentSessionID != nil {
		t.Fatalf("state = %+v, want failed, recoverable, no Session", st)
	}
	back := e.mustSelect("claude", false)
	if back.Status != "ready" || back.Desired != "claude" {
		t.Fatalf("switching back: %+v", back)
	}
}

// With the Agent back, a repeat recovers the original create contract: same request id, same identity.
func TestARecoverableFailureRecoversTheOriginalContractOnceTheAgentIsBack(t *testing.T) {
	e := providerEnv(t)
	stop, _ := e.warm()
	stop()
	e.svc.AfterIntent = func(context.Context) error {
		e.srv.RemoveAgent(agent.Namespace, codexAgentName)
		return nil
	}
	e.mustSelect("codex", false)
	e.svc.AfterIntent = nil
	failed := e.bindingRow()
	// While the Agent is missing neither a repeat nor a restart gets past the preflight.
	if code := e.putCode("codex", true); code != "agent_unavailable" {
		t.Fatalf("code = %q", code)
	}
	e.srv.AddAgent(agent.Namespace, codexAgentName)
	st := e.mustSelect("codex", false)
	if st.Status != "ready" || st.Recoverable || deref(e.bindingRow().KagentRequestID) != deref(failed.KagentRequestID) {
		t.Fatalf("state = %+v", st)
	}
}

// ---- 409 codes ----------------------------------------------------------------------------------

func TestEveryProviderConflictCarriesAStableCode(t *testing.T) {
	t.Run("busy", func(t *testing.T) {
		e := providerEnv(t)
		e.send("waiting")
		if code := e.putCode("codex", false); code != "busy" {
			t.Fatalf("code = %q", code)
		}
	})
	t.Run("pending", func(t *testing.T) {
		e := providerEnv(t)
		crashAfterIntent(t, e, "codex")
		if code := e.putCode("claude", false); code != "pending" {
			t.Fatalf("different provider: code = %q", code)
		}
		_, err := e.API().SendMessage(context.Background(), api.SendMessageRequestObject{Body: &api.ChatRequest{Message: "hello"}})
		if code := codeOf(t, err); code != "pending" {
			t.Fatalf("chat: code = %q", code)
		}
	})
	t.Run("unconfirmed", func(t *testing.T) {
		e := providerEnv(t)
		stop, _ := e.warm()
		stop()
		e.srv.TamperSession = func(proc string, s *kagentapi.Session) {
			s.Operation = kagentapi.RuntimeOperation_RUNTIME_OPERATION_SUSPEND
		}
		if code := e.putCode("codex", false); code != "unconfirmed" {
			t.Fatalf("code = %q", code)
		}
	})
	t.Run("unresolved", func(t *testing.T) {
		e := providerEnv(t)
		e.run()
		id := e.mainID()
		if _, err := e.pool.Exec(context.Background(),
			`UPDATE agent_bindings SET kagent_create_request_id = 'lost-reply' WHERE session_id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if code := e.putCode("codex", false); code != "unresolved" {
			t.Fatalf("code = %q", code)
		}
	})
	t.Run("agent_unavailable", func(t *testing.T) {
		e := providerEnv(t)
		e.srv.RemoveAgent(agent.Namespace, codexAgentName)
		if code := e.putCode("codex", false); code != "agent_unavailable" {
			t.Fatalf("code = %q", code)
		}
	})
}

// ---- no history for a main that never started --------------------------------------------------

func TestANeverStartedMainWritesNoHistoryButAnAbandonedTargetIsRecorded(t *testing.T) {
	e := providerEnv(t)
	e.mustSelect("codex", false) // never started
	if h := e.historyRows(); len(h) != 0 {
		t.Fatalf("history = %+v, want none", h)
	}
	e.mustSelect("claude", false) // the Codex target has a Session by now, so it is recorded
	if h := e.historyRows(); len(h) != 1 || h[0].Provider != "codex" {
		t.Fatalf("history = %+v, want the Codex identity", h)
	}
}

// ---- crashes and retries ------------------------------------------------------------------------

// The create landed and its reply was lost. The repeated PUT replays the frozen request id, kagent
// returns the Session that exists, and the main binds it: no second Session.
func TestACreateThatLandedWithItsReplyLostIsBoundByTheRepeat(t *testing.T) {
	e := providerEnv(t)
	crashAfterIntent(t, e, "codex")
	row := e.bindingRow()
	landed, err := e.other().CreateSession(context.Background(), &kagentapi.CreateSessionRequest{
		Agent:     &kagentapi.ResourceReference{Namespace: agent.Namespace, Name: codexAgentName},
		RequestId: deref(row.KagentRequestID), Name: "Mainloop main thread",
	})
	if err != nil {
		t.Fatal(err)
	}
	st := e.mustSelect("codex", false)
	if st.Status != "ready" || deref(st.KagentSessionID) != landed.GetId() {
		t.Fatalf("state = %+v, want the Session that landed, %s", st, landed.GetId())
	}
	if n := e.liveSessionsOn(codexAgentName); n != 1 {
		t.Fatalf("%d Sessions on the Codex Agent, want 1", n)
	}
}

// The old Session was suspended and the process died before the intent committed. Nothing durable
// moved, the main is still on its old provider, and a repeat finishes the switch.
func TestACrashAfterTheSuspendAndBeforeTheIntentLeavesTheOldProviderAndARepeatRecovers(t *testing.T) {
	e := providerEnv(t)
	stop, claudeKsid := e.warm()
	stop()
	before := e.bindingRow()
	errCrash := errors.New("crash after the suspend")
	e.svc.AfterSuspend = func(context.Context) error { return errCrash }
	if _, err := e.selectProvider("codex", false); !errors.Is(err, errCrash) {
		t.Fatalf("err = %v, want the crash", err)
	}
	e.svc.AfterSuspend = nil
	if e.ksState(claudeKsid) != kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED {
		t.Fatalf("old Session is %s", e.ksState(claudeKsid))
	}
	now := e.bindingRow()
	if now.Kind != before.Kind || deref(now.KagentSessionID) != deref(before.KagentSessionID) || len(e.historyRows()) != 0 {
		t.Fatalf("the binding moved before the intent: %+v", now)
	}
	st := e.mustSelect("codex", false)
	if st.Status != "ready" || st.Desired != "codex" || len(st.History) != 1 || deref(st.History[0].KagentSessionID) != claudeKsid {
		t.Fatalf("state after the repeat = %+v", st)
	}
	if e.createsOn(codexAgentName) != 1 {
		t.Fatalf("creates = %v", e.srv.CreateRequests())
	}
}

func TestConcurrentPUTsOfOneProviderCreateOneSession(t *testing.T) {
	e := providerEnv(t)
	stop, _ := e.warm()
	stop()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := e.svc.SelectMainProvider(context.Background(), "codex", false); err != nil && !errors.Is(err, sessions.ErrConflict) {
				t.Errorf("err = %v", err)
			}
		})
	}
	wg.Wait()
	st := e.mustSelect("codex", false)
	if st.Status != "ready" || len(st.History) != 1 {
		t.Fatalf("state = %+v", st)
	}
	if n := e.liveSessionsOn(codexAgentName); n != 1 {
		t.Fatalf("%d Codex Sessions, want 1", n)
	}
}

func TestConcurrentPUTsOfDifferentProvidersLeaveOneConsistentMain(t *testing.T) {
	e := providerEnv(t)
	stop, _ := e.warm()
	stop()
	var wg sync.WaitGroup
	for i := range 8 {
		provider := []string{"codex", "claude"}[i%2]
		wg.Go(func() {
			if _, err := e.svc.SelectMainProvider(context.Background(), provider, false); err != nil && !errors.Is(err, sessions.ErrConflict) {
				t.Errorf("err = %v", err)
			}
		})
	}
	wg.Wait()
	// Whatever won, repeating its selection settles it into one consistent main.
	st := e.mustSelect(e.bindingRow().Kind, false)
	if st.Status != "ready" {
		t.Fatalf("state = %+v", st)
	}
	if got := len(e.historyRows()); got != len(st.History) {
		t.Fatalf("history rows %d vs state %d", got, len(st.History))
	}
}

// ---- credentials per native identity ------------------------------------------------------------

func TestEachNativeIdentityHasItsOwnSecretAndTheSwitchDoesNotWaitForCleanup(t *testing.T) {
	e := providerEnv(t)
	a := e.withCredentials()
	stop, _ := e.warm()
	stop()
	main := e.mainID()
	old := e.credentialRows(main)
	if len(old) != 1 || old[0].State != dbstate.CredentialPublished {
		t.Fatalf("credentials before = %+v", old)
	}
	// No cleanup loop runs: the old Secret is still owed a delete when the target is created.
	start := time.Now()
	st := e.mustSelect("codex", false)
	if st.Status != "ready" || time.Since(start) > 5*time.Second {
		t.Fatalf("state = %+v after %s", st, time.Since(start))
	}
	var oldRow, newRow dbq.Credential
	for _, c := range e.credentialRows(main) {
		if c.State == dbstate.CredentialPublished {
			newRow = c
		} else {
			oldRow = c
		}
	}
	if oldRow.ID != old[0].ID || oldRow.State != dbstate.CredentialRevoked || newRow.ID == "" {
		t.Fatalf("credentials after = %+v", e.credentialRows(main))
	}
	if oldRow.SecretName == newRow.SecretName {
		t.Fatalf("both identities use the Secret %q", oldRow.SecretName)
	}
	// The Codex create references the new Secret, and the retired identity's token fails.
	reqs := e.srv.CreateRequests()
	last := reqs[len(reqs)-1]
	if got := last.GetCredentials()[0].GetSecretRef().GetName(); got != newRow.SecretName {
		t.Fatalf("create references %q, want %q", got, newRow.SecretName)
	}
	auth := &agentauth.Authenticator{Q: dbq.New(e.pool)}
	if _, err := auth.Authenticate(context.Background(), agentauth.Token(authtest.Key, oldRow.ID)); !errors.Is(err, agentauth.ErrUnauthorized) {
		t.Fatalf("the retired token: %v, want ErrUnauthorized", err)
	}
	if _, err := auth.Authenticate(context.Background(), agentauth.Token(authtest.Key, newRow.ID)); err != nil {
		t.Fatalf("the new token: %v", err)
	}
	// Cleaning up the old identity later does not touch the new Secret.
	runCredentialCleanup(t, e, a)
	e.waitFor("the old Secret is deleted", func() bool {
		for _, c := range e.credentialRows(main) {
			if c.ID == oldRow.ID {
				return c.State == dbstate.CredentialDeleted
			}
		}
		return true
	})
	if a.Cluster.UID(authtest.Namespace, newRow.SecretName) == "" || a.Cluster.UID(authtest.Namespace, oldRow.SecretName) != "" {
		t.Fatalf("new Secret present = %v, old Secret present = %v",
			a.Cluster.UID(authtest.Namespace, newRow.SecretName) != "", a.Cluster.UID(authtest.Namespace, oldRow.SecretName) != "")
	}
	if e.liveCredentialCount(main) != 1 {
		t.Fatalf("credentials = %+v", e.credentialRows(main))
	}
}
