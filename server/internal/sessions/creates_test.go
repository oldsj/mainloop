package sessions_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/sessions"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

// flaky is a kagent whose CreateSession can be switched to answer 503 (unavailable).
type flaky struct {
	sessions.Kagent
	down    atomic.Bool
	creates atomic.Int32
	last    atomic.Pointer[kagentapi.CreateSessionRequest]
}

func (f *flaky) CreateSession(ctx context.Context, r *kagentapi.CreateSessionRequest) (*kagentapi.Session, error) {
	f.creates.Add(1)
	f.last.Store(r)
	if f.down.Load() {
		return nil, &kagent.SessionError{Op: "CreateSession", Code: connect.CodeUnavailable, Detail: "503"}
	}
	return f.Kagent.CreateSession(ctx, r)
}

func (e *env) unresolved() int64 {
	e.t.Helper()
	n, err := dbq.New(e.pool).CountUnresolvedBindings(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) createRequested(sessionID string) bool {
	e.t.Helper()
	var set bool
	if err := e.pool.QueryRow(context.Background(),
		`SELECT kagent_create_request_id IS NOT NULL FROM agent_bindings WHERE session_id = $1`, sessionID).Scan(&set); err != nil {
		e.t.Fatal(err)
	}
	return set
}

// failedCreateThenCancelAndArchive is the reviewer's probe: the create keeps failing, the owner
// cancels the session and archives it. It returns the session id and the request id of the create.
func (e *env) failedCreateThenCancelAndArchive(k *flaky) (sessionID, requestID string) {
	e.t.Helper()
	ctx := context.Background()
	k.down.Store(true)
	sessionID = e.newSession("do it")
	e.run()
	deadline := time.Now().Add(15 * time.Second)
	for !e.createRequested(sessionID) || k.creates.Load() < 1 {
		if time.Now().After(deadline) {
			e.t.Fatal("the create was never attempted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := e.svc.Cancel(ctx, sessionID); err != nil {
		e.t.Fatal(err)
	}
	if err := e.svc.Archive(ctx, sessionID); err != nil {
		e.t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx,
		`SELECT kagent_create_request_id FROM agent_bindings WHERE session_id = $1`, sessionID).Scan(&requestID); err != nil {
		e.t.Fatal(err)
	}
	return sessionID, requestID
}

func newFlakyEnv(t *testing.T) (*env, *flaky) {
	t.Helper()
	e := newEnv(t)
	e.withProviders()
	k := &flaky{Kagent: e.svc.Kagent}
	e.svc.Kagent = k
	// kagent confirms a delete with the Session in state DELETED.
	e.srv.TamperSession = func(proc string, sess *kagentapi.Session) {
		if proc == kagent.ProcDeleteSession {
			sess.State = kagentapi.RuntimeState_RUNTIME_STATE_DELETED
		}
	}
	return e, k
}

// The create was requested and never answered; the session was cancelled and archived. Until the
// create is settled it counts as unresolved (kagent may hold a Session for it), and it must not
// count for ever.
func TestACancelledSessionWithAFailedCreateIsSettledByReplay(t *testing.T) {
	t.Run("kagent has no Session for it", func(t *testing.T) {
		e, k := newFlakyEnv(t)
		sid, _ := e.failedCreateThenCancelAndArchive(k)
		if e.unresolved() != 1 {
			t.Fatalf("%d unresolved, want the one create that may have landed", e.unresolved())
		}
		// kagent is still down: nothing can be settled, and nothing is lost.
		if err := e.svc.ResolveCreates(context.Background()); err == nil {
			t.Fatal("a replay that failed reported success")
		}
		if e.unresolved() != 1 {
			t.Fatal("an unsettled create stopped counting")
		}
		k.down.Store(false)
		if err := e.svc.ResolveCreates(context.Background()); err != nil {
			t.Fatal(err)
		}
		if e.unresolved() != 0 || e.createRequested(sid) {
			t.Fatal("the create is still unresolved after the replay")
		}
		// The replay made a Session in the fake (nothing existed) and deleted it again.
		if _, ok := e.srv.Session("session-1"); ok {
			t.Fatal("kagent still holds the Session the replay made")
		}
	})
	t.Run("the create had landed", func(t *testing.T) {
		e, k := newFlakyEnv(t)
		sid, req := e.failedCreateThenCancelAndArchive(k)
		k.down.Store(false)
		// The create reached kagent and only the reply was lost.
		landed, err := k.Kagent.CreateSession(context.Background(), k.last.Load())
		if err != nil {
			t.Fatal(err)
		}
		_ = req
		if err := e.svc.ResolveCreates(context.Background()); err != nil {
			t.Fatal(err)
		}
		if e.unresolved() != 0 {
			t.Fatal("still unresolved")
		}
		if _, ok := e.srv.Session(landed.GetId()); ok {
			t.Fatal("the Session the create made was kept")
		}
		if e.createRequested(sid) {
			t.Fatal("still marked")
		}
	})
	t.Run("the request id was reserved by a deleted Session", func(t *testing.T) {
		e, k := newFlakyEnv(t)
		sid, req := e.failedCreateThenCancelAndArchive(k)
		k.down.Store(false)
		_ = req
		landed, err := k.Kagent.CreateSession(context.Background(), k.last.Load())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := k.DeleteSession(context.Background(), landed.GetId()); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.ResolveCreates(context.Background()); err != nil {
			t.Fatal(err)
		}
		if e.unresolved() != 0 || e.createRequested(sid) {
			t.Fatal("still unresolved")
		}
	})
	t.Run("nothing is replayed while the session has an open turn", func(t *testing.T) {
		e, k := newFlakyEnv(t)
		k.down.Store(true)
		sid := e.newSession("do it")
		deadline := time.Now().Add(15 * time.Second)
		stop := e.run()
		defer stop()
		// The durable marker precedes the native call. Wait for that first call as well, so its
		// arrival during the sweep is not mistaken for a replay by the sweep.
		for !e.createRequested(sid) || k.creates.Load() < 1 {
			if time.Now().After(deadline) {
				t.Fatal("never attempted")
			}
			time.Sleep(10 * time.Millisecond)
		}
		before := k.creates.Load()
		if err := e.svc.ResolveCreates(context.Background()); err != nil {
			t.Fatal(err)
		}
		if k.creates.Load() != before {
			t.Fatal("the sweep replayed a create the delivery loop is still retrying")
		}
		if e.unresolved() != 1 {
			t.Fatal("the in-flight create stopped counting")
		}
	})
}

// A session whose create was never requested owns no kagent Session, so it never blocks the sweep.
func TestASessionThatNeverSentACreateIsNotUnresolved(t *testing.T) {
	t.Run("cancelled before any send", func(t *testing.T) {
		e := newEnv(t)
		e.withProviders()
		id := e.newSession("never sent") // no loop runs
		if _, err := e.svc.Cancel(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.Archive(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if e.unresolved() != 0 {
			t.Fatalf("%d unresolved", e.unresolved())
		}
	})
	t.Run("no provider, so no create is sent", func(t *testing.T) {
		e := newEnv(t)
		e.withProviders()
		id := e.newSession("no agent")
		e.svc.Providers = nil
		e.run()
		time.Sleep(300 * time.Millisecond) // the loop claims the delivery and cannot resolve an Agent
		if n := e.srv.Count(kagent.ProcCreateSession); n != 0 {
			t.Fatalf("%d creates sent", n)
		}
		if _, err := e.svc.Cancel(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.Archive(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if e.unresolved() != 0 || e.createRequested(id) {
			t.Fatal("a create that was never sent counts as unresolved")
		}
	})
	t.Run("the main thread before its first claim", func(t *testing.T) {
		e := newEnv(t)
		if _, err := e.svc.EnsureMain(context.Background()); err != nil {
			t.Fatal(err)
		}
		if e.unresolved() != 0 {
			t.Fatal("the unclaimed main binding is unresolved")
		}
	})
}

// A create that returned a Session stops counting once the Session id is stored.
func TestACreateThatReturnedASessionIsNotUnresolved(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("go")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	if e.unresolved() != 0 {
		t.Fatalf("%d unresolved", e.unresolved())
	}
}
