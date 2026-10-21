package sessions_test

import (
	"context"
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
