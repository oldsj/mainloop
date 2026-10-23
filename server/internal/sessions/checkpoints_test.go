package sessions_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
)

func codePtr(c connect.Code) *connect.Code { return &c }

// completedSession creates a standalone session and waits for its first (prompt) turn to
// complete, so it has a native kagent Session and a terminal task to checkpoint at.
func (e *env) completedSession() string {
	e.t.Helper()
	e.withProviders()
	e.run()
	id := e.newSession("hello")
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	return id
}

func (e *env) createCheckpoint(sessionID, requestID string) (api.Checkpoint, error) {
	e.t.Helper()
	r, err := e.API().CreateSessionCheckpoint(context.Background(), api.CreateSessionCheckpointRequestObject{
		SessionId: sessionID, Body: &api.CheckpointCreateRequest{RequestId: uuid.MustParse(requestID)},
	})
	if err != nil {
		return api.Checkpoint{}, err
	}
	return api.Checkpoint(r.(api.CreateSessionCheckpoint200JSONResponse)), nil
}

func (e *env) mustCheckpoint(sessionID string) api.Checkpoint {
	e.t.Helper()
	cp, err := e.createCheckpoint(sessionID, uuid.NewString())
	if err != nil {
		e.t.Fatal(err)
	}
	return cp
}

func (e *env) forkCheckpoint(checkpointID, requestID, title string, fresh bool) (api.CheckpointForkResponse, error) {
	e.t.Helper()
	body := &api.CheckpointForkRequest{RequestId: uuid.MustParse(requestID)}
	if fresh {
		body.FreshConversation = &fresh
	}
	if title != "" {
		body.Title = &title
	}
	r, err := e.API().ForkCheckpoint(context.Background(), api.ForkCheckpointRequestObject{CheckpointId: checkpointID, Body: body})
	if err != nil {
		return api.CheckpointForkResponse{}, err
	}
	return api.CheckpointForkResponse(r.(api.ForkCheckpoint200JSONResponse)), nil
}

// ---- checkpoint create ----------------------------------------------------------------------

func TestCreateCheckpointIsIdempotent(t *testing.T) {
	e := newEnv(t)
	id := e.completedSession()
	rid := uuid.NewString()

	first, err := e.createCheckpoint(id, rid)
	if err != nil {
		t.Fatal(err)
	}
	if first.Id == "" || first.SessionId == "" || first.State != api.CheckpointStateReady {
		t.Fatalf("checkpoint %+v", first)
	}
	if n := e.srv.Count(kagent.ProcCreateCheckpoint); n != 1 {
		t.Fatalf("CreateCheckpoint called %d times", n)
	}

	// A repeat does not create a second checkpoint: it reads the recorded one back (and its live
	// state, with one GetCheckpoint), never calling CreateCheckpoint again.
	again, err := e.createCheckpoint(id, rid)
	if err != nil || again.Id != first.Id {
		t.Fatalf("repeat %+v %v", again, err)
	}
	if n := e.srv.Count(kagent.ProcCreateCheckpoint); n != 1 {
		t.Fatalf("CreateCheckpoint called %d times after repeat", n)
	}
	if n := e.srv.Count(kagent.ProcGetCheckpoint); n != 1 {
		t.Fatalf("GetCheckpoint called %d times after repeat", n)
	}
}

// After kagent's CreateCheckpoint outcome is uncertain (a 502 with no reply), a repeat of the same
// request is kagent's own idempotent replay, not a second checkpoint: CreateCheckpoint runs again
// for the same request id and kagent, having never completed it, makes exactly one.
func TestCreateCheckpointRetryAfterUncertainReconciles(t *testing.T) {
	e := newEnv(t)
	id := e.completedSession()
	rid := uuid.NewString()

	e.srv.FailNext(kagent.ProcCreateCheckpoint, kagenttest.Fault{Status: 502})
	_, err := e.createCheckpoint(id, rid)
	if bad, ok := apierr.As(err); !ok || bad.Status != 502 {
		t.Fatalf("status %v, err %v", bad, err)
	}

	out, err := e.createCheckpoint(id, rid)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if out.Id == "" {
		t.Fatal("retry did not complete the checkpoint")
	}
	if n := e.srv.Count(kagent.ProcCreateCheckpoint); n != 2 {
		t.Fatalf("CreateCheckpoint called %d times", n)
	}
}

func TestCreateCheckpointErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		code       connect.Code
		wantStatus int
	}{
		{"invalid argument", connect.CodeInvalidArgument, 422},
		{"not found", connect.CodeNotFound, 404},
		{"failed precondition", connect.CodeFailedPrecondition, 409},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.completedSession()
			e.srv.FailNext(kagent.ProcCreateCheckpoint, kagenttest.Fault{GRPC: codePtr(c.code)})
			_, err := e.createCheckpoint(id, uuid.NewString())
			bad, ok := apierr.As(err)
			if !ok || bad.Status != c.wantStatus {
				t.Fatalf("status %v, err %v", bad, err)
			}
		})
	}
}

func TestCreateCheckpointRefusesWhileATurnIsOpen(t *testing.T) {
	e := newEnv(t)
	id := e.completedSession()
	e.openTurn(id, "more")
	_, err := e.createCheckpoint(id, uuid.NewString())
	if bad, ok := apierr.As(err); !ok || bad.Status != 409 {
		t.Fatalf("status %v, err %v", bad, err)
	}
}

// ---- fork ------------------------------------------------------------------------------------

func TestForkIsIdempotent(t *testing.T) {
	e := newEnv(t)
	id := e.completedSession()
	cp := e.mustCheckpoint(id)
	rid := uuid.NewString()

	first, err := e.forkCheckpoint(cp.Id, rid, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if first.NewSessionId == "" || first.AcceptedAt == nil {
		t.Fatalf("fork %+v", first)
	}
	if n := e.srv.Count(kagent.ProcForkSession); n != 1 {
		t.Fatalf("ForkSession called %d times", n)
	}
	if e.bindingKsid(first.NewSessionId) == "" {
		t.Fatal("the forked session has no kagent session id")
	}

	again, err := e.forkCheckpoint(cp.Id, rid, "", false)
	if err != nil || again.NewSessionId != first.NewSessionId {
		t.Fatalf("repeat %+v %v", again, err)
	}
	if n := e.srv.Count(kagent.ProcForkSession); n != 1 {
		t.Fatalf("ForkSession called %d times after repeat", n)
	}
}

// After kagent's ForkSession outcome is uncertain, a repeat of the same request calls ForkSession
// again for the same request id (kagent's own idempotent replay), rather than leaving the fork
// stuck or starting a second one.
func TestForkRetryAfterUncertainReconciles(t *testing.T) {
	e := newEnv(t)
	id := e.completedSession()
	cp := e.mustCheckpoint(id)
	rid := uuid.NewString()

	e.srv.FailNext(kagent.ProcForkSession, kagenttest.Fault{Status: 502})
	_, err := e.forkCheckpoint(cp.Id, rid, "", false)
	if bad, ok := apierr.As(err); !ok || bad.Status != 502 {
		t.Fatalf("status %v, err %v", bad, err)
	}

	out, err := e.forkCheckpoint(cp.Id, rid, "", false)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if out.NewSessionId == "" {
		t.Fatal("retry did not complete the fork")
	}
	if n := e.srv.Count(kagent.ProcForkSession); n != 2 {
		t.Fatalf("ForkSession called %d times", n)
	}
}

func TestForkErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		code       connect.Code
		wantStatus int
	}{
		{"invalid argument", connect.CodeInvalidArgument, 422},
		{"not found", connect.CodeNotFound, 404},
		{"failed precondition", connect.CodeFailedPrecondition, 409},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.completedSession()
			cp := e.mustCheckpoint(id)
			e.srv.FailNext(kagent.ProcForkSession, kagenttest.Fault{GRPC: codePtr(c.code)})
			_, err := e.forkCheckpoint(cp.Id, uuid.NewString(), "", false)
			bad, ok := apierr.As(err)
			if !ok || bad.Status != c.wantStatus {
				t.Fatalf("status %v, err %v", bad, err)
			}
		})
	}
}

// ---- fork workspace binding (issue #311) ------------------------------------------------------

// makeWorkspace turns an existing session into a workspace manifest: a git checkout with dev
// settings, the shape workspaces.Service.Create would leave on the row. It is built directly with
// SQL (as ready_test.go also does) rather than wiring the whole workspaces.Service into this
// package's tests, because checkpoints.go only ever reads the row through dbq.
func (e *env) makeWorkspace(sessionID, branch string) {
	e.t.Helper()
	ctx := context.Background()
	projectID := "proj-" + sessionID
	if _, err := e.pool.Exec(ctx,
		`INSERT INTO projects (id,user_id,owner,name,full_name,html_url) VALUES ($1,$2,'o','n','o/n','https://github.com/o/n')`,
		projectID, owner); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx,
		`INSERT INTO workspaces
		    (id,user_id,project_id,repo_url,ref,branch,agent_kind,agent_namespace,agent_name,ports,idle_timeout_minutes,environment,request_id)
		 VALUES ($1,$2,$3,'https://github.com/o/n','main',$4,'claude','kagent','mainloop-main',
		         '[{"name":"app","number":3000}]',45,
		         '{"environment_id":"e1","version_id":"v1","platform":"linux/arm64","image":"img@sha256:x","policy_identity":"v1:val"}',
		         $5)`,
		sessionID, owner, projectID, branch, "req-"+sessionID); err != nil {
		e.t.Fatal(err)
	}
}

// workspaceManifest reads back the fields the fork's workspace row should have copied, and
// reports whether the row exists at all.
func (e *env) workspaceManifest(id string) (repoURL, branch, agentKind, agentNamespace string, idleMinutes int32, ports, environment string, ok bool) {
	e.t.Helper()
	row := e.pool.QueryRow(context.Background(),
		`SELECT repo_url, branch, agent_kind, agent_namespace, idle_timeout_minutes, ports, environment
		 FROM workspaces WHERE id=$1`, id)
	var portsJSON, envJSON []byte
	if err := row.Scan(&repoURL, &branch, &agentKind, &agentNamespace, &idleMinutes, &portsJSON, &envJSON); err != nil {
		return "", "", "", "", 0, "", "", false
	}
	return repoURL, branch, agentKind, agentNamespace, idleMinutes, string(portsJSON), string(envJSON), true
}

func (e *env) workspaceOpCount(id string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM workspace_ops WHERE workspace_id=$1`, id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// A fork of a session that is itself a workspace gets its own workspace row, with the source's
// manifest copied, and no create operation: the binding's forked kagent Session id is already
// bound (checkpoints.go's commitFork sets it in the same transaction), so GET /workspaces/{fork},
// suspend, resume, idle suspend and delete all work through the ordinary workspace path with no
// create ever sent for it.
func TestForkOfAWorkspaceCreatesAWorkspaceRow(t *testing.T) {
	e := newEnv(t)
	id := e.completedSession()
	e.makeWorkspace(id, "feature-x")
	cp := e.mustCheckpoint(id)

	out, err := e.forkCheckpoint(cp.Id, uuid.NewString(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if out.NewSessionId == "" {
		t.Fatal("fork did not return a new session id")
	}

	srcRepoURL, srcBranch, srcKind, srcNamespace, srcIdle, srcPorts, srcEnv, ok := e.workspaceManifest(id)
	if !ok {
		t.Fatal("the source workspace row is missing")
	}
	repoURL, branch, agentKind, agentNamespace, idleMinutes, ports, environment, ok := e.workspaceManifest(out.NewSessionId)
	if !ok {
		t.Fatal("the fork has no workspace row")
	}
	// branch is the one field makeWorkspace gave the fork's source deliberately different from a
	// fresh default, so it is compared against that literal; everything else is compared against
	// the source's own row, read back the same way (jsonb re-serializes ports and environment in
	// its own canonical form, so a literal here would be comparing against the wrong thing).
	if repoURL != srcRepoURL || branch != "feature-x" || agentKind != srcKind ||
		agentNamespace != srcNamespace || idleMinutes != srcIdle || ports != srcPorts || environment != srcEnv {
		t.Fatalf("manifest not copied from source (repo=%s branch=%s kind=%s ns=%s idle=%d ports=%s env=%s):\ngot  repo=%s branch=%s kind=%s ns=%s idle=%d ports=%s env=%s",
			srcRepoURL, srcBranch, srcKind, srcNamespace, srcIdle, srcPorts, srcEnv,
			repoURL, branch, agentKind, agentNamespace, idleMinutes, ports, environment)
	}
	if e.bindingKsid(out.NewSessionId) == "" {
		t.Fatal("the forked workspace's binding has no kagent session id")
	}
	if n := e.workspaceOpCount(out.NewSessionId); n != 0 {
		t.Fatalf("a fork's workspace needs no create operation; found %d", n)
	}
}

// A fork of a plain session (not every checkpointed session is a workspace) gets no workspace
// row: there is no manifest to copy, and nothing in this change should invent one.
func TestForkOfAPlainSessionGetsNoWorkspaceRow(t *testing.T) {
	e := newEnv(t)
	id := e.completedSession()
	cp := e.mustCheckpoint(id)

	out, err := e.forkCheckpoint(cp.Id, uuid.NewString(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, _, ok := e.workspaceManifest(out.NewSessionId); ok {
		t.Fatal("a fork of a non-workspace session should not get a workspace row")
	}
}

// ---- checkpoint delete truthfulness (issue #311) -----------------------------------------------

func TestDeleteCheckpointAlreadyGoneIsStill200(t *testing.T) {
	e := newEnv(t)
	id := e.completedSession()
	cp := e.mustCheckpoint(id)
	// Deleted directly through kagent, behind Mainloop's back, so the next delete finds it gone.
	if err := e.other().DeleteCheckpoint(context.Background(), cp.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.API().DeleteCheckpoint(context.Background(), api.DeleteCheckpointRequestObject{CheckpointId: cp.Id}); err != nil {
		t.Fatalf("already-gone delete: %v", err)
	}
}

// kagent's BeginDeleteSessionCheckpoint answers NotFound both for a checkpoint that is genuinely
// gone and for one a fork still pins (issue #311). DeleteCheckpoint must not read that NotFound as
// "already gone" without checking: it re-reads the checkpoint, finds it still there, and answers
// 409 naming the pinning fork instead of 200.
func TestDeleteCheckpointStillPinnedByAForkIs409(t *testing.T) {
	e := newEnv(t)
	id := e.completedSession()
	cp := e.mustCheckpoint(id)
	forked, err := e.forkCheckpoint(cp.Id, uuid.NewString(), "", false)
	if err != nil {
		t.Fatal(err)
	}

	e.srv.FailNext(kagent.ProcDeleteCheckpoint, kagenttest.Fault{GRPC: codePtr(connect.CodeNotFound)})
	_, err = e.API().DeleteCheckpoint(context.Background(), api.DeleteCheckpointRequestObject{CheckpointId: cp.Id})
	bad, ok := apierr.As(err)
	if !ok || bad.Status != 409 {
		t.Fatalf("status %v, err %v", bad, err)
	}
	if !strings.Contains(bad.Message, forked.NewSessionId) {
		t.Fatalf("detail does not name the pinning fork %s: %q", forked.NewSessionId, bad.Message)
	}
	// The checkpoint must still be there: it was never actually deleted.
	if _, err := e.API().GetCheckpoint(context.Background(), api.GetCheckpointRequestObject{CheckpointId: cp.Id}); err != nil {
		t.Fatalf("checkpoint should still exist: %v", err)
	}
}

// Without a known pinning fork recorded, the 409 still names the reason in general terms.
func TestDeleteCheckpointStillPinnedWithNoKnownForkNamesItGenerically(t *testing.T) {
	e := newEnv(t)
	id := e.completedSession()
	cp := e.mustCheckpoint(id)

	e.srv.FailNext(kagent.ProcDeleteCheckpoint, kagenttest.Fault{GRPC: codePtr(connect.CodeNotFound)})
	_, err := e.API().DeleteCheckpoint(context.Background(), api.DeleteCheckpointRequestObject{CheckpointId: cp.Id})
	bad, ok := apierr.As(err)
	if !ok || bad.Status != 409 {
		t.Fatalf("status %v, err %v", bad, err)
	}
	if !strings.Contains(bad.Message, "pinned by a fork") {
		t.Fatalf("detail: %q", bad.Message)
	}
}
