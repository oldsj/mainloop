package workspaces

import (
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

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
	err := &kagent.SessionError{Op: "ResumeSession", Code: connect.CodeFailedPrecondition, Detail: "Agent does not have a ready prepared revision"}
	perm, reason := classify(err)
	if perm || reason != "kagent is not ready for this change" {
		t.Fatalf("%v %q", perm, reason)
	}
}

func TestAnOperationOnAWorkspaceDeletedMeanwhileIsNotFound(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.svc.insertOp(bg, dbq.New(h.svc.Pool), "no-such-workspace", dbstate.OpSuspend, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

// An open delete that lands in the gap between the refusal and the drop is as much a reason to keep
// the rows as an open create: the delete decides what happens to them.
func TestDropRejectedLeavesAWorkspaceWithAnOpenDelete(t *testing.T) {
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

func TestWakeOfAWorkspaceBeingDeletedOrGoneIsRefused(t *testing.T) {
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

// kagent parks and wakes a workspace's Actor itself, so Wake only records the use: it starts no
// operation, for a running workspace or a sleeping one.
func TestWakeTouchesAndStartsNoOperation(t *testing.T) {
	h := newHarness(t, nil)
	w := h.running()
	h.exec(`UPDATE workspaces SET last_active_at = now() - interval '1 hour' WHERE id = $1`, w.WorkspaceId)
	for range 2 {
		if err := h.svc.Wake(bg, w.WorkspaceId); err != nil {
			t.Fatal(err)
		}
	}
	got, err := h.svc.Get(bg, w.WorkspaceId)
	if err != nil || got.LastActivityAt == nil || time.Since(*got.LastActivityAt) > time.Minute {
		t.Fatalf("not touched: %+v %v", got.LastActivityAt, err)
	}
	if _, err := dbq.New(h.svc.Pool).GetOpenWorkspaceOp(bg, w.WorkspaceId); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Wake opened an operation: %v", err)
	}
}

// A suspend or resume operation queued before kagent took over sleeping has nothing to do: the loop
// cancels it without a kagent call.
func TestAQueuedSuspendOrResumeIsCancelled(t *testing.T) {
	h := newHarness(t, nil)
	w := h.running()
	for _, kind := range []dbstate.WorkspaceOpKind{dbstate.OpSuspend, dbstate.OpResume} {
		op, err := h.svc.enqueue(bg, w.WorkspaceId, kind, nil)
		if err != nil {
			t.Fatal(err)
		}
		stop := h.run()
		final, err := h.svc.await(bg, op)
		stop()
		if err != nil || final.State != dbstate.OpCancelled {
			t.Fatalf("%s: %+v %v", kind, final, err)
		}
	}
}
