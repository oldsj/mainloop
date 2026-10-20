package sessions_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

func (e *env) restart(sessionID, requestID string) (api.SessionRestartResponse, error) {
	e.t.Helper()
	r, err := e.API().RestartSession(context.Background(), api.RestartSessionRequestObject{
		SessionId: sessionID, Body: &api.SessionRestartRequest{RequestId: uuid.MustParse(requestID)},
	})
	if err != nil {
		return api.SessionRestartResponse{}, err
	}
	return api.SessionRestartResponse(r.(api.RestartSession200JSONResponse)), nil
}

func (e *env) restartState(id string) string {
	e.t.Helper()
	var st string
	if err := e.pool.QueryRow(context.Background(), `SELECT state FROM session_restarts WHERE id = $1`, id).Scan(&st); err != nil {
		e.t.Fatal(err)
	}
	return st
}

// The 2026-10-18 incident: the main thread's turn is wedged inside kagent, CancelTask never confirms,
// and the owner's next message waits behind it. A restart deletes the native Session, fails the turn
// (not uncertain, so nothing holds the queue) and the queued message runs on a new Session that
// carries the standing context again.
func TestRestartRecoversAMainTurnThatCannotBeStopped(t *testing.T) {
	e := newEnv(t)
	_, oldKsid := e.warm()
	main := e.mainID()
	mid := e.openTurn(main, "search the web")
	queued := uuid.NewString()
	if sent, err := e.svc.Send(context.Background(), queued, "", "are you there?"); err != nil || !sent.Queued {
		t.Fatalf("send behind the wedged turn: %+v %v", sent, err)
	}

	e.srv.FailNext("CancelTask", kagenttest.Fault{RPC: &kagenttest.RPCError{Code: -32603, Message: "A2A CancelTask outcome unknown (timeout)"}})
	if _, err := e.API().StopTurn(context.Background(), api.StopTurnRequestObject{SessionId: main}); err == nil {
		t.Fatal("the stop of a wedged turn succeeded")
	} else if bad, ok := apierr.As(err); !ok || bad.Status != 502 {
		t.Fatalf("stop: %v", err)
	}

	native, err := e.API().GetMainThread(context.Background(), api.GetMainThreadRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	if n := api.MainThreadInfo(native.(api.GetMainThread200JSONResponse)).Native; n == nil || n.Restartable == nil || !*n.Restartable {
		t.Fatalf("the main thread is not offered a restart: %+v", n)
	}

	rid := uuid.NewString()
	got, err := e.restart(main, rid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != api.SessionRestartResponseStatus(sessions.Restarted) || len(got.FailedMessageIds) != 1 || got.FailedMessageIds[0] != mid {
		t.Fatalf("restart %+v", got)
	}
	if d := e.delivery(mid); d.State != dbstate.DeliveryFailed || !strings.Contains(deref(d.Detail), "restarted") {
		t.Fatalf("wedged turn %s %q", d.State, deref(d.Detail))
	}
	if _, ok := e.srv.Session(oldKsid); ok {
		t.Fatal("the wedged native session was not deleted")
	}
	// The queued message moves on at once, on a new Session, carrying the recent conversation.
	d := e.waitState(queued, dbstate.DeliveryCompleted)
	if ksid := e.bindingKsid(main); ksid == "" || ksid == oldKsid {
		t.Fatalf("binding session %q, old %q", ksid, oldKsid)
	}
	// (This env carries no earlier messages, so the new Session gets the context block alone.)
	if d.SentText == nil || !strings.Contains(*d.SentText, "[mainloop:context]") || !strings.Contains(*d.SentText, "are you there?") {
		t.Fatalf("the first message to the new Session carried %q", deref(d.SentText))
	}
	if e.restartState(rid) != "done" {
		t.Fatal("the restart intent is not done")
	}

	// Repeating the request returns the recorded outcome and deletes nothing more.
	deletes := e.srv.Count(kagent.ProcDeleteSession)
	again, err := e.restart(main, rid)
	if err != nil || again.Status != got.Status || len(again.FailedMessageIds) != 1 || again.FailedMessageIds[0] != mid {
		t.Fatalf("repeat %+v %v", again, err)
	}
	if e.srv.Count(kagent.ProcDeleteSession) != deletes {
		t.Fatal("a repeated restart deleted again")
	}
	if e.delivery(queued).State != dbstate.DeliveryCompleted {
		t.Fatal("a repeated restart touched the newer turn")
	}
}

// A delete kagent does not confirm leaves the turn as it was and the intent requested; repeating
// the same request finishes it, and the resume sweep would too.
func TestRestartWhoseDeleteIsUnconfirmedIsContinuedByARepeat(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	ksid := e.bindingKsid(id)
	mid := e.openTurn(id, "long")

	code := connect.CodeUnavailable
	e.srv.FailNext(kagent.ProcDeleteSession, kagenttest.Fault{GRPC: &code})
	rid := uuid.NewString()
	_, err := e.restart(id, rid)
	if bad, ok := apierr.As(err); !ok || bad.Status != 502 || !strings.HasPrefix(bad.Message, "Could not restart") {
		t.Fatalf("got %v", err)
	}
	if d := e.delivery(mid); d.State != dbstate.DeliveryDelivered {
		t.Fatalf("an unconfirmed restart wrote the turn: %s", d.State)
	}
	if e.restartState(rid) != "requested" {
		t.Fatal("the intent is not kept")
	}
	// A different request id while one is open continues the open one instead of starting another.
	got, err := e.restart(id, uuid.NewString())
	if err != nil || got.Status != api.SessionRestartResponseStatus(sessions.Restarted) || len(got.FailedMessageIds) != 1 {
		t.Fatalf("continue %+v %v", got, err)
	}
	if _, ok := e.srv.Session(ksid); ok {
		t.Fatal("the session was not deleted")
	}
	if e.restartState(rid) != "done" {
		t.Fatal("the open intent was not the one finished")
	}
	e.waitState(mid, dbstate.DeliveryFailed)
}

// The sweep finishes a restart whose request was cut off after the intent committed. It reads kagent
// first: a Session already gone is not deleted again.
func TestRestartSweepFinishesAnInterruptedRestart(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	ksid := e.bindingKsid(id)
	mid := e.openTurn(id, "long")

	rid := uuid.NewString()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO session_restarts (id, session_id, kagent_session_id, updated_at) VALUES ($1, $2, $3, now() - interval '5 minutes')`,
		rid, id, ksid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Kagent.DeleteSession(context.Background(), ksid); err != nil {
		t.Fatal(err)
	}
	deletes := e.srv.Count(kagent.ProcDeleteSession)
	if err := e.svc.ResumeRestarts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.restartState(rid) != "done" || e.delivery(mid).State != dbstate.DeliveryFailed {
		t.Fatalf("restart %s, turn %s", e.restartState(rid), e.delivery(mid).State)
	}
	if e.srv.Count(kagent.ProcDeleteSession) != deletes {
		t.Fatal("the sweep deleted a Session kagent already showed gone")
	}
}

func TestRestartIsRefusedForAnEndedSessionOrABadRequestID(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	if _, err := e.svc.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.restart(id, uuid.NewString()); err == nil {
		t.Fatal("restarted a cancelled session")
	} else if bad, ok := apierr.As(err); !ok || bad.Status != 409 {
		t.Fatalf("got %v", err)
	}
	if _, err := e.svc.Restart(context.Background(), id, "not-a-uuid"); err == nil {
		t.Fatal("took a request id that is not a UUID")
	}
	if _, err := e.restart(uuid.NewString(), uuid.NewString()); err == nil {
		t.Fatal("restarted an unknown session")
	} else if bad, ok := apierr.As(err); !ok || bad.Status != 404 {
		t.Fatalf("got %v", err)
	}
	n, err := e.API().GetSessionNative(context.Background(), api.GetSessionNativeRequestObject{SessionId: id})
	if err != nil {
		t.Fatal(err)
	}
	if info := api.NativeSessionInfo(n.(api.GetSessionNative200JSONResponse)); info.Restartable == nil || *info.Restartable {
		t.Fatalf("a cancelled session reads restartable: %+v", info.Restartable)
	}
}

// A turn whose task and Session kagent no longer has (deleted out of band, as the owner's manual
// recovery did) fails at once, instead of becoming uncertain and holding the queue for the hold.
func TestATurnWhoseNativeSessionIsGoneFailsAtOnce(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	ksid := e.bindingKsid(id)

	mid := uuid.NewString()
	task := runningTask("t-lost", ksid, mid, "partial")
	e.srv.AddTask(task)
	// The stream ends while the task still works, so the loop follows it by reading it again.
	e.srv.Script(kagenttest.Emit{Event: task}, kagenttest.Drop{})
	if _, err := e.svc.SendTo(context.Background(), id, mid, "go"); err != nil {
		t.Fatal(err)
	}
	e.waitState(mid, dbstate.DeliveryDelivered)
	queued := uuid.NewString()
	if sent, err := e.svc.SendTo(context.Background(), id, queued, "next"); err != nil || !sent.Queued {
		t.Fatalf("%+v %v", sent, err)
	}

	e.srv.DropTask("t-lost")
	if _, err := e.svc.Kagent.DeleteSession(context.Background(), ksid); err != nil {
		t.Fatal(err)
	}
	d := e.waitState(mid, dbstate.DeliveryFailed)
	if !strings.Contains(deref(d.Detail), "no longer exists") {
		t.Fatalf("detail %q", deref(d.Detail))
	}
	// Not uncertain: the queued message is admitted and runs on a replacement Session at once.
	start := time.Now()
	e.waitState(queued, dbstate.DeliveryCompleted)
	if time.Since(start) > 10*time.Second {
		t.Fatal("the queue waited")
	}
	if e.bindingKsid(id) == ksid {
		t.Fatal("the deleted Session was not replaced")
	}
}
