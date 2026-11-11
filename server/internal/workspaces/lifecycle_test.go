package workspaces

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

var bg = context.Background()

// slow keeps retries far apart so a test can look at the state between attempts, and keeps the
// request wait short.
var slow = map[string]string{
	"WORKSPACE_CREATE_RETRY_INITIAL_SECONDS": "30",
	"WORKSPACE_CREATE_RETRY_MAX_SECONDS":     "30",
	"WORKSPACE_OPERATION_WAIT_SECONDS":       "0.2",
}

var kagenttestDrop = kagenttest.Fault{Drop: true}

func grpcFault(c connect.Code) kagenttest.Fault { return kagenttest.Fault{GRPC: &c} }

// due makes the workspace's open operation retry now.
func (h *harness) due() {
	h.t.Helper()
	h.exec(`UPDATE workspace_ops SET next_attempt_at = now() WHERE state IN ('pending','retrying')`)
}

// Mined from backend/tests/runtime/test_workspaces.py: test_create_sends_the_workspace.
func TestCreateBindsOneSessionAndPublishes(t *testing.T) {
	h := newHarness(t, nil)
	w := h.running()
	ss := h.sessions()
	if len(ss) != 1 {
		t.Fatalf("sessions: %d", len(ss))
	}
	row := h.row(w.WorkspaceId)
	got := ss[0].GetWorkspace()
	if got.GetRepo() != "https://github.com/oldsj/mainloop" || got.GetRef() != row.Ref ||
		!strings.HasPrefix(got.GetBranch(), "mainloop/") || got.GetBranch() != row.Branch {
		t.Fatalf("workspace sent to kagent: %v (row %+v)", got, row)
	}
	if row.KagentSessionID == nil || *row.KagentSessionID != ss[0].GetId() {
		t.Fatalf("binding %v, kagent %s", row.KagentSessionID, ss[0].GetId())
	}
	if ss[0].GetAgent().GetRef().GetName() != h.cfg.Kagent.WorkspaceClaudeAgent {
		t.Fatalf("agent %v", ss[0].GetAgent())
	}
	if w.SessionId != w.WorkspaceId || w.PublicationMode != api.ReadOnly || *w.PublicationReason != api.Disabled {
		t.Fatalf("%+v", w)
	}
	if n := h.events(); n != 2 { // accepted, then bound
		t.Fatalf("workspace:updated events: %d", n)
	}
	if h.op(w.WorkspaceId, "create").State != dbstate.OpSucceeded {
		t.Fatal("create op not succeeded")
	}
}

// Mined: test_create_is_not_repeated_once_the_session_exists, test_a_lost_reply_is_retried_and_recovers_the_same_session.
// The first attempt reached kagent and its reply was lost with the process; the replacement
// runner must get that Session back, not make a second.
func TestCrashMidCreateResumesWithoutSecondSession(t *testing.T) {
	h := newHarness(t, map[string]string{"WORKSPACE_OPERATION_WAIT_SECONDS": "0.1"})
	w, err := h.create(api.CreateWorkspaceRequest{}) // no runner: the op stays queued
	if err != nil || w.ObservedState != api.WorkspaceObservedStateResuming {
		t.Fatalf("%v %+v", err, w)
	}
	first, err := h.client.CreateSession(bg, h.row(w.WorkspaceId).createRequest()) // the lost attempt
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE workspace_ops SET lease_owner = 'dead-runner', lease_until = now() - interval '1 second', attempts = 1
	        WHERE workspace_id = $1`, w.WorkspaceId)

	h.run()
	eventually(t, "binding", func() bool { return h.row(w.WorkspaceId).KagentSessionID != nil })
	if id := *h.row(w.WorkspaceId).KagentSessionID; id != first.GetId() {
		t.Fatalf("bound %s, first attempt made %s", id, first.GetId())
	}
	if n := len(h.sessions()); n != 1 {
		t.Fatalf("kagent has %d sessions", n)
	}
}

// Mined: test_an_unknown_create_outcome_keeps_the_rows_for_refresh,
// test_unavailable_then_success_runs_without_a_manual_refresh.
func TestUnknownOutcomeIsRetriedByARestartedRunner(t *testing.T) {
	h := newHarness(t, map[string]string{
		"WORKSPACE_CREATE_RETRY_INITIAL_SECONDS": "0.5", "WORKSPACE_CREATE_RETRY_MAX_SECONDS": "0.5",
		"WORKSPACE_OPERATION_WAIT_SECONDS": "0.1",
	})
	h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
	stop := h.run()
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if w.ObservedState != api.WorkspaceObservedStateResuming || w.Detail == nil || !strings.Contains(*w.Detail, "retrying automatically") {
		t.Fatalf("%+v", w)
	}
	eventually(t, "first failure recorded", func() bool { return h.op(w.WorkspaceId, "create").Failures == 1 })
	stop() // the process dies between attempts
	if h.row(w.WorkspaceId).KagentSessionID != nil || len(h.sessions()) != 0 {
		t.Fatal("nothing should exist yet")
	}
	h.run() // its replacement carries on without a refresh
	eventually(t, "binding", func() bool { return h.row(w.WorkspaceId).KagentSessionID != nil })
	if n := len(h.sessions()); n != 1 {
		t.Fatalf("kagent has %d sessions", n)
	}
	if n := h.fake.Count(kagent.ProcCreateSession); n != 2 {
		t.Fatalf("create calls: %d", n)
	}
}

// Mined: test_backoff_doubles_to_the_cap.
func TestBackoffDoublesToTheCap(t *testing.T) {
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Config: cfg}
	var got []time.Duration
	for n := 1; n <= 6; n++ {
		got = append(got, s.retryDelay(n))
	}
	want := []time.Duration{15, 30, 60, 120, 120, 120}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("delays %v", got)
		}
	}
	if cfg.Workspaces.CreateRetryWindow.Duration() != 600*time.Second {
		t.Fatalf("window %v", cfg.Workspaces.CreateRetryWindow.Duration())
	}
}

// Mined: test_retries_give_up_after_the_window_and_refresh_restarts_them,
// test_a_failed_manual_refresh_after_a_stop_rearms_retries.
func TestRetriesGiveUpAfterTheWindowAndRefreshRestartsThem(t *testing.T) {
	h := newHarness(t, slow)
	h.run()
	h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	op := h.op(w.WorkspaceId, "create")
	if op.State != dbstate.OpRetrying || op.FirstFailedAt == nil {
		t.Fatalf("%+v", op)
	}
	h.exec(`UPDATE workspace_ops SET first_failed_at = now() - interval '601 seconds'`)
	h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
	h.due()
	eventually(t, "gave up", func() bool { return h.op(w.WorkspaceId, "create").State == dbstate.OpGaveUp })
	got, err := h.svc.Get(bg, w.WorkspaceId)
	if err != nil || got.ObservedState != api.WorkspaceObservedStateFailed || !strings.Contains(*got.Detail, "Refresh") {
		t.Fatalf("%v %+v", err, got)
	}

	// A refresh that fails again re-arms automatic retries with a fresh window.
	h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
	got, err = h.svc.Refresh(bg, w.WorkspaceId)
	if err != nil || got.ObservedState != api.WorkspaceObservedStateResuming {
		t.Fatalf("%v %+v", err, got)
	}
	op = h.op(w.WorkspaceId, "create")
	if op.State != dbstate.OpRetrying || op.Failures != 1 {
		t.Fatalf("%+v", op)
	}
	h.due()
	eventually(t, "bound", func() bool { return h.row(w.WorkspaceId).KagentSessionID != nil })
	if n := len(h.sessions()); n != 1 {
		t.Fatalf("sessions %d", n)
	}
}

// Mined: test_a_permanent_error_stops_without_retrying, test_a_rejected_first_create_still_removes_the_rows.
func TestPermanentErrorRemovesTheRows(t *testing.T) {
	h := newHarness(t, nil)
	h.run()
	h.fake.FailNext(kagent.ProcCreateSession, grpcFault(connect.CodeInvalidArgument))
	_, err := h.create(api.CreateWorkspaceRequest{})
	var rej *RejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("%v", err)
	}
	if h.workspaceCount() != 0 || h.fake.Count(kagent.ProcCreateSession) != 1 {
		t.Fatalf("rows %d, create calls %d", h.workspaceCount(), h.fake.Count(kagent.ProcCreateSession))
	}
}

// Mined: test_a_retry_racing_a_manual_refresh_creates_one_session.
func TestRefreshRacingTheLoopCreatesOneSession(t *testing.T) {
	h := newHarness(t, slow)
	h.run()
	h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	h.due()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { _, _ = h.svc.Refresh(bg, w.WorkspaceId) })
	}
	wg.Wait()
	eventually(t, "binding", func() bool { return h.row(w.WorkspaceId).KagentSessionID != nil })
	if n := len(h.sessions()); n != 1 {
		t.Fatalf("sessions %d", n)
	}
}

func TestAnotherOperationOpenIsBusy(t *testing.T) {
	h := newHarness(t, map[string]string{"WORKSPACE_OPERATION_WAIT_SECONDS": "0.1"})
	w, err := h.create(api.CreateWorkspaceRequest{}) // create stays queued: no runner
	if err != nil {
		t.Fatal(err)
	}
	// Preview traffic is not held while the workspace is still being created.
	if err := h.svc.WakeForPreview(bg, w.WorkspaceId); !errors.Is(err, ErrBusy) {
		t.Fatalf("%v", err)
	}
	// A second create joins nothing: one operation at a time.
	if _, err := h.svc.enqueue(bg, w.WorkspaceId, dbstate.OpResume, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("%v", err)
	}
}

// Mined: test_workspace_delete_removes_rows_only_after_kagent_confirms,
// test_an_unconfirmed_archive_delete_is_retried_by_the_reconcile_pass.
func TestDeleteKeepsRowsUntilKagentConfirms(t *testing.T) {
	h := newHarness(t, slow)
	w := h.running()
	h.run()
	h.deleteState.Store(int32(kagentapi.RuntimeState_RUNTIME_STATE_READY)) // kagent answers without confirming
	err := h.svc.Delete(bg, w.WorkspaceId)
	var unc *UnconfirmedError
	if !errors.As(err, &unc) || h.workspaceCount() != 1 {
		t.Fatalf("%v, rows %d", err, h.workspaceCount())
	}
	h.due() // the retry finds the Session gone
	eventually(t, "rows removed", func() bool { return h.workspaceCount() == 0 })
}

// Mined: test_workspace_delete_with_kagent_unreachable_keeps_rows.
func TestDeleteWithKagentUnreachableKeepsRows(t *testing.T) {
	h := newHarness(t, slow)
	w := h.running()
	h.run()
	h.fake.FailNext(kagent.ProcDeleteSession, kagenttest.Fault{Drop: true})
	err := h.svc.Delete(bg, w.WorkspaceId)
	var unc *UnconfirmedError
	if !errors.As(err, &unc) || h.workspaceCount() != 1 || len(h.sessions()) != 1 {
		t.Fatalf("%v, rows %d", err, h.workspaceCount())
	}
	// Delete returned after the short wait, maybe before the runner tried; let the failed attempt
	// happen first, or due() is a no-op and the 30 s backoff outlasts the wait below.
	eventually(t, "failure recorded", func() bool { return h.op(w.WorkspaceId, "delete").Failures == 1 })
	h.due()
	eventually(t, "rows removed", func() bool { return h.workspaceCount() == 0 })
	if len(h.sessions()) != 0 {
		t.Fatal("session left behind")
	}
}

// Mined: test_workspace_delete_refuses_while_a_turn_is_open.
func TestDeleteRefusesWhileATurnIsOpen(t *testing.T) {
	h := newHarness(t, nil)
	w := h.running()
	h.run()
	h.dl.setOpen(w.WorkspaceId, true)
	var rej *RejectedError
	if err := h.svc.Delete(bg, w.WorkspaceId); !errors.As(err, &rej) || h.workspaceCount() != 1 {
		t.Fatalf("%v", err)
	}
}

// Mined: test_archive_with_nothing_created_has_nothing_to_delete: a workspace whose create was
// never sent is removed without calling kagent.
func TestDeleteBeforeAnyCreateAttempt(t *testing.T) {
	h := newHarness(t, map[string]string{"WORKSPACE_OPERATION_WAIT_SECONDS": "0.1"})
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Create returned with no runner, so its operation is still queued and due. Park it so the
	// runner started below cannot claim and send it before Delete cancels it.
	h.exec(`UPDATE workspace_ops SET next_attempt_at = now() + interval '1 hour' WHERE kind = 'create'`)
	h.run()
	// Delete waits for the runner to claim and finish the delete, which the runner is free to take
	// its time over; the short wait above was for Create, which has no runner.
	defer h.patient(15 * time.Second)()
	if err := h.svc.Delete(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	if h.workspaceCount() != 0 || h.fake.Count(kagent.ProcCreateSession) != 0 || h.fake.Count(kagent.ProcDeleteSession) != 0 {
		t.Fatalf("rows %d, create %d, delete %d", h.workspaceCount(), h.fake.Count(kagent.ProcCreateSession), h.fake.Count(kagent.ProcDeleteSession))
	}
}

// Mined: test_delete_of_an_unknown_create_resolves_it_and_deletes_the_session.
func TestDeleteOfAnUnknownCreateDeletesTheSession(t *testing.T) {
	h := newHarness(t, map[string]string{"WORKSPACE_OPERATION_WAIT_SECONDS": "0.1"})
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.CreateSession(bg, h.row(w.WorkspaceId).createRequest()); err != nil { // the attempt whose reply was lost
		t.Fatal(err)
	}
	h.exec(`UPDATE workspace_ops SET attempts = 1`)
	h.run()
	h.svc.Config.Workspaces.OperationWait = config.Seconds(5 * time.Second)
	if err := h.svc.Delete(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	if h.workspaceCount() != 0 || len(h.sessions()) != 0 {
		t.Fatalf("rows %d, sessions %d", h.workspaceCount(), len(h.sessions()))
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func TestStateMapping(t *testing.T) {
	S, O := kagentapi.RuntimeState_value, kagentapi.RuntimeOperation_value
	cases := []struct {
		state, op string
		want      api.WorkspaceObservedState
		detail    bool
		awake     bool
		asleep    bool
	}{
		{state: "RUNTIME_STATE_READY", op: "RUNTIME_OPERATION_NONE", want: api.WorkspaceObservedStateRunning, awake: true},
		// Asleep: READY with an association that is not current.
		{state: "RUNTIME_STATE_READY", op: "RUNTIME_OPERATION_NONE", want: api.WorkspaceObservedStateSuspended, asleep: true},
		// No association: kagent did not say, and the Session runs on use.
		{state: "RUNTIME_STATE_READY", op: "RUNTIME_OPERATION_NONE", want: api.WorkspaceObservedStateRunning},
		// kagent's own lifecycle work is brief: the workspace is on its way to running.
		{state: "RUNTIME_STATE_READY", op: "RUNTIME_OPERATION_RESUME", want: api.WorkspaceObservedStateResuming},
		{state: "RUNTIME_STATE_READY", op: "RUNTIME_OPERATION_SUSPEND", want: api.WorkspaceObservedStateResuming},
		{state: "RUNTIME_STATE_READY", op: "RUNTIME_OPERATION_REVERT", want: api.WorkspaceObservedStateResuming},
		{state: "RUNTIME_STATE_SUSPENDED", op: "RUNTIME_OPERATION_NONE", want: api.WorkspaceObservedStateResuming},
		{state: "RUNTIME_STATE_CREATING", op: "RUNTIME_OPERATION_CREATE", want: api.WorkspaceObservedStateResuming, detail: true},
		{state: "RUNTIME_STATE_FAILED", op: "RUNTIME_OPERATION_NONE", want: api.WorkspaceObservedStateFailed, detail: true},
		{state: "RUNTIME_STATE_DELETING", op: "RUNTIME_OPERATION_DELETE", want: api.WorkspaceObservedStateUnknown, detail: false},
		{state: "RUNTIME_STATE_DELETED", op: "RUNTIME_OPERATION_NONE", want: api.WorkspaceObservedStateUnknown, detail: false},
		{state: "RUNTIME_STATE_UNSPECIFIED", op: "RUNTIME_OPERATION_UNSPECIFIED", want: api.WorkspaceObservedStateUnknown, detail: false},
	}
	for _, c := range cases {
		sess := &kagentapi.Session{State: kagentapi.RuntimeState(S[c.state]), Operation: kagentapi.RuntimeOperation(O[c.op])}
		if c.asleep {
			sess.RuntimeAssociation = &kagentapi.RuntimeAssociation{CurrentActive: false}
		}
		if c.awake {
			sess.RuntimeAssociation = &kagentapi.RuntimeAssociation{CurrentActive: true}
		}
		got, detail := stateOf(sess)
		if got != c.want || (detail != nil) != c.detail {
			t.Errorf("%s/%s: %s %v", c.state, c.op, got, detail)
		}
	}
	// A failure carries kagent's reason.
	for _, f := range []*kagentapi.Failure{{Reason: "ImagePullBackOff", Message: "no image"}, {Reason: "ImagePullBackOff"}} {
		got, detail := stateOf(&kagentapi.Session{State: kagentapi.RuntimeState_RUNTIME_STATE_FAILED, Failure: f})
		want := cmpOr(f.Message, f.Reason)
		if got != api.WorkspaceObservedStateFailed || detail == nil || *detail != want {
			t.Errorf("%s %v, want %q", got, detail, want)
		}
	}
}

// Mined: test_unreachable_kagent_is_unknown_not_an_error, test_a_session_kagent_no_longer_has_is_unknown.
func TestKagentDownShowsUnknown(t *testing.T) {
	h := newHarness(t, nil)
	w := h.running()
	h.fake.FailNext(kagent.ProcGetSession, kagenttest.Fault{Drop: true})
	got, err := h.svc.Get(bg, w.WorkspaceId)
	if err != nil || got.ObservedState != api.WorkspaceObservedStateUnknown {
		t.Fatalf("%v %+v", err, got)
	}
	h.exec(`UPDATE agent_bindings SET kagent_session_id = 'gone' WHERE session_id = $1`, w.WorkspaceId)
	got, err = h.svc.Get(bg, w.WorkspaceId)
	if err != nil || got.ObservedState != api.WorkspaceObservedStateUnknown {
		t.Fatalf("%v %+v", err, got)
	}
}
