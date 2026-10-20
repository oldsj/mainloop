package workspaces

import (
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
)

// ---- G5 follow-up 2: delete of a workspace kagent was not ready for ---------------------------------------

// kagent answers FAILED_PRECONDITION for an Agent that is not ready as well as for a deleted
// request id. For a delete's replayed create either one is an answer given after the lookup found no
// Session holding the id, so the rows go.
func TestDeleteOfANotReadyWorkspacePurgesTheRows(t *testing.T) {
	t.Parallel()
	h := newHarness(t, slow)
	h.run()
	h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	h.fake.FailNext(kagent.ProcCreateSession, failedPrecondition("Agent does not have a ready prepared revision"))
	h.svc.Config.Workspaces.OperationWait = config.Seconds(5e9)
	if err := h.svc.Delete(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	if h.workspaceCount() != 0 {
		t.Fatal("the rows of a workspace that never got a Session were kept")
	}
	if n := len(h.sessions()); n != 0 {
		t.Fatalf("%d Sessions in kagent", n)
	}
}

func TestReplayFoundNoSessionIsOneRule(t *testing.T) {
	t.Parallel()
	session := func(code connect.Code, detail string) error {
		return &kagent.SessionError{Op: "CreateSession", Code: code, Detail: detail}
	}
	for name, tt := range map[string]struct {
		err  error
		want bool
	}{
		"deleted request id":     {session(connect.CodeFailedPrecondition, "request_id belongs to a deleted Session"), true},
		"agent not ready":        {session(connect.CodeFailedPrecondition, "Agent does not have a ready prepared revision"), true},
		"invalid argument":       {session(connect.CodeInvalidArgument, "bad request"), true},
		"unavailable":            {session(connect.CodeUnavailable, "try later"), false},
		"internal":               {session(connect.CodeInternal, "boom"), false},
		"not a session error":    {errors.New("dial tcp: refused"), false},
		"no error (it was made)": {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := replayFoundNoSession(tt.err); got != tt.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestTheNotReadyReasonNamesNoOperation(t *testing.T) {
	t.Parallel()
	err := &kagent.SessionError{Op: "ResumeSession", Code: connect.CodeFailedPrecondition, Detail: "Agent does not have a ready prepared revision"}
	perm, reason := classify(err)
	if perm || reason != "kagent is not ready for this change" {
		t.Fatalf("%v %q", perm, reason)
	}
}

func TestAnOperationOnAWorkspaceDeletedMeanwhileIsNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	_, err := h.svc.insertOp(bg, dbq.New(h.svc.Pool), "no-such-workspace", dbstate.OpSuspend, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

// An open delete that lands in the gap between the refusal and the drop is as much a reason to keep
// the rows as an open create: the delete decides what happens to them.
func TestDropRejectedLeavesAWorkspaceWithAnOpenDelete(t *testing.T) {
	t.Parallel()
	h := newHarness(t, slow)
	h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
	stop := h.run()
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	h.exec(`UPDATE workspace_ops SET state = 'rejected', lease_owner = NULL, lease_until = NULL WHERE id = $1`, h.op(w.WorkspaceId, "create").ID)
	refused := h.op(w.WorkspaceId, "create")

	if _, err := h.svc.enqueue(bg, w.WorkspaceId, dbstate.OpDelete, nil); err != nil { // the delete in the gap
		t.Fatal(err)
	}
	if err := h.svc.dropRejected(bg, w.WorkspaceId, refused); err != nil {
		t.Fatal(err)
	}
	if h.workspaceCount() != 1 {
		t.Fatal("the rows were dropped from under an open delete")
	}
	// Once nothing is open, the refused create is the latest and the rows go.
	h.exec(`UPDATE workspace_ops SET state = 'cancelled' WHERE workspace_id = $1 AND state IN ('pending', 'retrying')`, w.WorkspaceId)
	if err := h.svc.dropRejected(bg, w.WorkspaceId, refused); err != nil {
		t.Fatal(err)
	}
	if h.workspaceCount() != 0 {
		t.Fatal("rows kept with nothing open")
	}
	// A workspace that is already gone is not an error.
	if err := h.svc.dropRejected(bg, w.WorkspaceId, refused); err != nil {
		t.Fatalf("dropping a missing workspace: %v", err)
	}
}

// ---- Wake -----------------------------------------------------------------------------------------------------

func TestWakeWaitsForAnOpenSuspendThenResumes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]string{"WORKSPACE_OPERATION_WAIT_SECONDS": "10"})
	w := h.running()
	if _, err := h.svc.enqueue(bg, w.WorkspaceId, dbstate.OpSuspend, nil); err != nil { // no runner: it stays open
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.svc.Wake(bg, w.WorkspaceId) }()

	select {
	case err := <-done:
		t.Fatalf("Wake returned while the suspend was open: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if h.fake.Count(kagent.ProcResumeSession) != 0 {
		t.Fatal("resumed under an open suspend")
	}
	h.run()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Wake never returned")
	}
	if h.fake.Count(kagent.ProcSuspendSession) != 1 || h.fake.Count(kagent.ProcResumeSession) != 1 {
		t.Fatalf("%d suspends, %d resumes", h.fake.Count(kagent.ProcSuspendSession), h.fake.Count(kagent.ProcResumeSession))
	}
	got, _ := h.svc.Get(bg, w.WorkspaceId)
	if got.ObservedState != api.WorkspaceObservedStateRunning {
		t.Fatalf("%+v", got)
	}
}

func TestWakeResumesASuspendedWorkspaceOnceAndLeavesARunningOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	w := h.running()
	h.run()
	if err := h.svc.Wake(bg, w.WorkspaceId); err != nil || h.fake.Count(kagent.ProcResumeSession) != 0 {
		t.Fatalf("a running workspace was resumed: %v", err)
	}
	if _, err := h.svc.Suspend(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := h.svc.Wake(bg, w.WorkspaceId); err != nil {
			t.Fatal(err)
		}
	}
	if n := h.fake.Count(kagent.ProcResumeSession); n != 1 {
		t.Fatalf("%d resumes", n)
	}
}

func TestWakeOfAWorkspaceBeingDeletedOrGoneIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	w := h.running()
	if _, err := h.svc.enqueue(bg, w.WorkspaceId, dbstate.OpDelete, nil); err != nil {
		t.Fatal(err)
	}
	var rej *RejectedError
	if err := h.svc.Wake(bg, w.WorkspaceId); !errors.As(err, &rej) {
		t.Fatalf("got %v", err)
	}
	if err := h.svc.Wake(bg, "no-such-workspace"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

// ---- G6b follow-up 3: a resume that finds the workspace running ---------------------------------------------

// A resume operation that finds the Session already ready changes nothing, so it publishes no
// workspace:updated event. Callers that read "suspended" just before another resume finished queue
// exactly such an operation.
func TestResumeOfARunningWorkspacePublishesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	w := h.running()
	h.run()
	events := h.events()
	if _, err := h.svc.Resume(bg, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	if n := h.fake.Count(kagent.ProcResumeSession); n != 0 || h.events() != events {
		t.Fatalf("resume calls %d, events %d -> %d", n, events, h.events())
	}
}

// A retried resume that finds the Session ready may be finishing a resume whose reply was lost, which
// nothing else announced: it publishes.
func TestRetriedResumeOfARunningWorkspacePublishes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	w := h.running()
	op, err := h.svc.enqueue(bg, w.WorkspaceId, dbstate.OpResume, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE workspace_ops SET failures = 1 WHERE id = $1`, op.ID)
	events := h.events()
	h.run()
	if final, err := h.svc.await(bg, op); err != nil || final.State != dbstate.OpSucceeded {
		t.Fatalf("%+v %v", final, err)
	}
	if h.events() != events+1 {
		t.Fatalf("events %d -> %d", events, h.events())
	}
}
