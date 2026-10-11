package workspaces

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// A refusal that lands after Create already returned keeps the rows (an earlier attempt's outcome
// is unknown), and a later delete must not be stuck behind the same refusal.
func TestDeleteAfterALateRefusalPurgesTheRows(t *testing.T) {
	for name, fault := range map[string]kagenttest.Fault{
		"permanent":  grpcFault(connect.CodeInvalidArgument),
		"deleted-id": failedPrecondition("request_id belongs to a deleted Session"),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, slow)
			h.run()
			h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
			w, err := h.create(api.CreateWorkspaceRequest{})
			if err != nil {
				t.Fatal(err)
			}
			h.fake.FailNext(kagent.ProcCreateSession, fault)
			h.due()
			eventually(t, "refused", func() bool { return h.op(w.WorkspaceId, "create").State == dbstate.OpRejected })
			if h.workspaceCount() != 1 {
				t.Fatalf("the rows of a create with an unknown first attempt were dropped")
			}
			got, err := h.svc.Get(bg, w.WorkspaceId)
			if err != nil || got.ObservedState != api.WorkspaceObservedStateFailed {
				t.Fatalf("%v %+v", err, got)
			}
			h.fake.FailNext(kagent.ProcCreateSession, fault) // the delete replays the create
			h.svc.Config.Workspaces.OperationWait = config.Seconds(5e9)
			if err := h.svc.Delete(bg, w.WorkspaceId); err != nil {
				t.Fatal(err)
			}
			if h.workspaceCount() != 0 {
				t.Fatal("rows kept")
			}
		})
	}
}

func failedPrecondition(message string) kagenttest.Fault {
	f := grpcFault(connect.CodeFailedPrecondition)
	f.Message = message
	return f
}

// kagent answers FAILED_PRECONDITION for an Agent that is not ready yet as well as for a deleted
// request id; only the second is permanent, so the first must keep retrying.
func TestNotReadyFailedPreconditionRetries(t *testing.T) {
	for _, message := range []string{
		"Agent does not have a ready prepared revision",
		"golden preparation failed",
		"composition is not configured",
	} {
		t.Run(message, func(t *testing.T) {
			h := newHarness(t, map[string]string{
				"WORKSPACE_CREATE_RETRY_INITIAL_SECONDS": "0.2", "WORKSPACE_CREATE_RETRY_MAX_SECONDS": "0.2",
				"WORKSPACE_OPERATION_WAIT_SECONDS": "0.1",
			})
			h.fake.FailNext(kagent.ProcCreateSession, failedPrecondition(message))
			h.run()
			w, err := h.create(api.CreateWorkspaceRequest{})
			if err != nil {
				t.Fatalf("a transient refusal failed the create: %v", err)
			}
			if w.ObservedState != api.WorkspaceObservedStateResuming {
				t.Fatalf("%+v", w)
			}
			eventually(t, "binding", func() bool { return h.row(w.WorkspaceId).KagentSessionID != nil })
			if h.workspaceCount() != 1 || h.fake.Count(kagent.ProcCreateSession) != 2 {
				t.Fatalf("rows %d, create calls %d", h.workspaceCount(), h.fake.Count(kagent.ProcCreateSession))
			}
		})
	}
}

func TestFailedPreconditionClassification(t *testing.T) {
	deleted := &kagent.SessionError{Op: "CreateSession", Code: connect.CodeFailedPrecondition, Detail: "request_id belongs to a deleted Session"}
	if perm, _ := classify(deleted); !perm {
		t.Fatal("a deleted request id must be permanent")
	}
	notReady := &kagent.SessionError{Op: "CreateSession", Code: connect.CodeFailedPrecondition, Detail: "Agent does not have a ready prepared revision"}
	if perm, _ := classify(notReady); perm {
		t.Fatal("an Agent that is not ready must be transient")
	}
}

// A Refresh that lands between the refusal being recorded and Create dropping the rows adds a
// create that may already have been sent; the drop must leave it alone.
func TestDropRejectedLeavesAWorkspaceWithANewerOperation(t *testing.T) {
	h := newHarness(t, slow)
	h.fake.FailNext(kagent.ProcCreateSession, kagenttest.Fault{Drop: true})
	stop := h.run()
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	refused := h.op(w.WorkspaceId, "create")
	h.exec(`UPDATE workspace_ops SET state = 'rejected', lease_owner = NULL, lease_until = NULL WHERE id = $1`, refused.ID)
	refused = h.op(w.WorkspaceId, "create")

	if _, err := h.svc.enqueue(bg, w.WorkspaceId, dbstate.OpCreate, nil); err != nil { // the Refresh in the gap
		t.Fatal(err)
	}
	if err := h.svc.dropRejected(bg, w.WorkspaceId, refused); err != nil {
		t.Fatal(err)
	}
	if h.workspaceCount() != 1 {
		t.Fatal("the rows of a workspace with a newer create were dropped")
	}

	h.exec(`UPDATE workspace_ops SET state = 'cancelled' WHERE workspace_id = $1 AND state IN ('pending', 'retrying')`, w.WorkspaceId)
	if err := h.svc.dropRejected(bg, w.WorkspaceId, refused); err != nil {
		t.Fatal(err)
	}
	if h.workspaceCount() != 1 {
		t.Fatal("dropped on the strength of an op that is no longer the latest create")
	}
	// The newest create refused in its turn: nothing is open, and it is the latest, so it drops.
	h.exec(`UPDATE workspace_ops SET state = 'rejected' WHERE id = $1`, h.op(w.WorkspaceId, "create").ID)
	if err := h.svc.dropRejected(bg, w.WorkspaceId, h.op(w.WorkspaceId, "create")); err != nil {
		t.Fatal(err)
	}
	if h.workspaceCount() != 0 {
		t.Fatal("rows kept for the latest refused create")
	}
}

// A refusal of the very first request drops the rows and says so on the event stream.
func TestFirstAttemptRefusalDropsTheRowsAndPublishes(t *testing.T) {
	h := newHarness(t, nil)
	h.run()
	before := h.events()
	h.fake.FailNext(kagent.ProcCreateSession, failedPrecondition("request_id belongs to a deleted Session"))
	if _, err := h.create(api.CreateWorkspaceRequest{}); err == nil {
		t.Fatal("expected a refusal")
	}
	if h.workspaceCount() != 0 {
		t.Fatal("rows kept")
	}
	if got := h.events(); got <= before {
		t.Fatalf("no workspace:updated for the dropped workspace (%d -> %d)", before, got)
	}
}

func TestScheduledOrphanSweepLogsWhenBlocked(t *testing.T) {
	h := newHarness(t, map[string]string{"WORKSPACE_OPERATION_WAIT_SECONDS": "0.1"})
	h.strayOn(h.cfg.Kagent.Namespace, h.cfg.Kagent.WorkspaceClaudeAgent, "stray-1")
	if _, err := h.create(api.CreateWorkspaceRequest{}); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE workspace_ops SET attempts = 1`)
	h.svc.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if err := h.svc.scheduledOrphanSweep(bg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.logs.String(), "orphan sweep blocked") {
		t.Fatalf("not logged: %s", h.logs.String())
	}
}

// Interim: without the delivery ledger a running turn is invisible, so idle suspend stays off.
func TestIdleSweepIsOffWithoutADeliveryLedger(t *testing.T) {
	names := func(h *harness) map[string]bool {
		out := map[string]bool{}
		for _, sw := range h.svc.sweeps() {
			out[sw.Name] = true
		}
		return out
	}
	h := newHarness(t, nil)
	if got := names(h); !got["workspace-idle"] || !got["workspace-archived"] || !got["workspace-orphans"] {
		t.Fatalf("%v", got)
	}
	h.svc.Deliveries = nil
	got := names(h)
	if got["workspace-idle"] || !got["workspace-archived"] || !got["workspace-orphans"] {
		t.Fatalf("%v", got)
	}
	if !strings.Contains(h.logs.String(), "idle suspend is off") {
		t.Fatalf("not logged: %s", h.logs.String())
	}
}

// A session kagent cannot suspend marks the workspace, so later passes do not queue it again.
func TestIdleSuspendOfAFailedSessionDoesNotChurn(t *testing.T) {
	h := newHarness(t, nil)
	w := h.running()
	h.backdate(w.WorkspaceId)
	h.getState.Store(int32(kagentapi.RuntimeState_RUNTIME_STATE_FAILED))
	h.run()
	for range 3 {
		if err := h.svc.SuspendIdle(bg); err != nil {
			t.Fatal(err)
		}
		eventually(t, "settled", func() bool { return h.op(w.WorkspaceId, "suspend").State != dbstate.OpPending })
	}
	if n := h.suspendOps(); n != 1 {
		t.Fatalf("%d suspend ops", n)
	}
	if h.fake.Count(kagent.ProcSuspendSession) != 0 {
		t.Fatal("suspended a failed session")
	}
	if st := h.op(w.WorkspaceId, "suspend").State; st != dbstate.OpRejected {
		t.Fatalf("op %s", st)
	}
}

func TestIdleSuspendOfAVanishedSessionDoesNotChurn(t *testing.T) {
	h := newHarness(t, nil)
	w := h.running()
	h.backdate(w.WorkspaceId)
	h.fake.FailNext(kagent.ProcGetSession, grpcFault(connect.CodeNotFound))
	h.run()
	for range 2 {
		if err := h.svc.SuspendIdle(bg); err != nil {
			t.Fatal(err)
		}
		eventually(t, "settled", func() bool { return h.op(w.WorkspaceId, "suspend").State != dbstate.OpPending })
	}
	if n := h.suspendOps(); n != 1 {
		t.Fatalf("%d suspend ops", n)
	}
}

// The agent is fixed when the workspace is made: a config change between attempts neither changes
// the replayed request nor hides the Session from the orphan sweep.
func TestAgentIsStoredWithTheWorkspace(t *testing.T) {
	h := newHarness(t, slow)
	w, err := h.create(api.CreateWorkspaceRequest{}) // no runner
	if err != nil {
		t.Fatal(err)
	}
	row := h.row(w.WorkspaceId)
	if row.AgentNamespace != h.cfg.Kagent.Namespace || row.AgentName != h.cfg.Kagent.WorkspaceClaudeAgent {
		t.Fatalf("%s/%s", row.AgentNamespace, row.AgentName)
	}
	h.exec(`UPDATE workspaces SET agent_namespace = 'earlier', agent_name = 'earlier-agent' WHERE id = $1`, w.WorkspaceId)
	req := h.row(w.WorkspaceId).createRequest()
	if req.GetAgent().GetNamespace() != "earlier" || req.GetAgent().GetName() != "earlier-agent" {
		t.Fatalf("replay used %v", req.GetAgent())
	}
	own, err := h.svc.ownAgents(bg)
	if err != nil || !own["earlier/earlier-agent"] || !own[h.cfg.Kagent.Namespace+"/"+h.cfg.Kagent.WorkspaceClaudeAgent] {
		t.Fatalf("%v %v", err, own)
	}
}

// A create reply without a Session id is not a binding.
func TestDeleteReplayWithoutASessionIDIsNotConfirmed(t *testing.T) {
	h := newHarness(t, map[string]string{"WORKSPACE_OPERATION_WAIT_SECONDS": "0.1"})
	h.fake.TamperSession = func(proc string, s *kagentapi.Session) {
		if proc == kagent.ProcCreateSession {
			s.Id = ""
		}
	}
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE workspace_ops SET attempts = 1`)
	h.run()
	if err := h.svc.Delete(bg, w.WorkspaceId); err == nil {
		t.Fatal("delete succeeded on a reply without a session id")
	}
	if h.workspaceCount() != 1 || h.row(w.WorkspaceId).KagentSessionID != nil {
		t.Fatal("bound or dropped an empty session id")
	}
}
