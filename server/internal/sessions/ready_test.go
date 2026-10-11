package sessions_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

func TestReadySessionResumesASuspendedSession(t *testing.T) {
	e := newEnv(t)
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	ksid := e.kagentSessionID()
	if _, err := e.other().SuspendSession(context.Background(), ksid); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ReadySession(context.Background(), "s1", nil, ksid); err != nil {
		t.Fatal(err)
	}
	if n := e.srv.Count(kagent.ProcResumeSession); n != 1 {
		t.Fatalf("%d resumes", n)
	}
	// Ready already: nothing more to do.
	if err := e.svc.ReadySession(context.Background(), "s1", nil, ksid); err != nil || e.srv.Count(kagent.ProcResumeSession) != 1 {
		t.Fatalf("ready session: %v, %d resumes", err, e.srv.Count(kagent.ProcResumeSession))
	}
}

// wakeFake stands in for the workspace service: waking the workspace resumes its Session.
type wakeFake struct {
	resume func() error
	wakes  atomic.Int32
}

func (w *wakeFake) Wake(context.Context, string) error {
	w.wakes.Add(1)
	return w.resume()
}

func TestReadySessionWakesTheWorkspaceBehindASuspendedSession(t *testing.T) {
	e := newEnv(t)
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	ksid := e.kagentSessionID()
	if _, err := e.other().SuspendSession(context.Background(), ksid); err != nil {
		t.Fatal(err)
	}
	w := &wakeFake{resume: func() error {
		_, err := e.other().ResumeSession(context.Background(), ksid)
		return err
	}}
	e.svc.Waker = w
	ws := "ws1"
	if err := e.svc.ReadySession(context.Background(), "ws1", &ws, ksid); err != nil {
		t.Fatal(err)
	}
	if w.wakes.Load() != 1 {
		t.Fatalf("%d wakes", w.wakes.Load())
	}
	// A workspace is resumed through its own wake, never directly.
	if n := e.srv.Count(kagent.ProcResumeSession); n != 1 {
		t.Fatalf("%d resumes (the wake's own)", n)
	}
}

func TestReadySessionFailsWhenTheWorkspaceStaysAsleep(t *testing.T) {
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.ReadyWait = 100 * time.Millisecond })
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	ksid := e.kagentSessionID()
	if _, err := e.other().SuspendSession(context.Background(), ksid); err != nil {
		t.Fatal(err)
	}
	e.svc.Waker = &wakeFake{resume: func() error { return nil }} // wakes, but the Session stays suspended
	ws := "ws1"
	if err := e.svc.ReadySession(context.Background(), "ws1", &ws, ksid); err == nil {
		t.Fatal("a Session that is still suspended after the wake is not ready")
	}
	if err := e.svc.ReadySession(context.Background(), "ws1", nil, ""); err == nil {
		t.Fatal("no kagent session is not ready")
	}
}
