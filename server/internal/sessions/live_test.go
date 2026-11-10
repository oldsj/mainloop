package sessions_test

import (
	"context"
	"errors"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/liveactor"
	"github.com/oldsj/mainloop/server/internal/sessions"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

// liveEnv is a main session on a live Agent, with Actors on in the fake kagent.
func liveEnv(t *testing.T) (e *env, mainID, ksid string) {
	t.Helper()
	e = newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.LiveAgents = []string{agent.Name} })
	e.srv.Actors = true
	e.svc.ActorAtespace = "kagent"
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	ksid = e.kagentSessionID()
	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return e, m.SessionID, ksid
}

// The actor is the one kagent's runtime association names, awake or asleep, and nothing wakes a
// sleeping Session here: the relay's connection through the router does.
func TestLiveTargetNamesTheActorAwakeOrAsleepAndWakesNothing(t *testing.T) {
	e, mainID, ksid := liveEnv(t)
	want := liveactor.Target{KagentSessionID: ksid, Actor: "ate/" + kagent.ActorName(ksid)}
	if got, err := e.svc.LiveTarget(context.Background(), mainID); err != nil || got != want {
		t.Fatalf("awake: %+v %v, want %+v", got, err, want)
	}
	e.srv.Sleep(ksid)
	if got, err := e.svc.LiveTarget(context.Background(), mainID); err != nil || got != want {
		t.Fatalf("asleep: %+v %v, want %+v", got, err, want)
	}
	if n := e.srv.Wakes(ksid); n != 0 {
		t.Fatalf("the lookup woke the Session %d times", n)
	}
}

// Without a runtime association (kagent at the vendored pin reports none) the Actor is the one the
// Session's A2A authority names.
func TestLiveTargetWithoutAnAssociationIsTheA2AAuthoritysActor(t *testing.T) {
	e, mainID, ksid := liveEnv(t)
	e.srv.NoAssociation = true
	got, err := e.svc.LiveTarget(context.Background(), mainID)
	if err != nil || got.Actor != "ate/"+kagent.ActorName(ksid) {
		t.Fatalf("%+v %v", got, err)
	}
}

// A Session that names its Actor neither way is named by kagent's stable Actor name in the
// configured atespace.
func TestLiveTargetFallsBackToTheStableActorName(t *testing.T) {
	e, mainID, ksid := liveEnv(t)
	e.srv.TamperSession = func(_ string, s *kagentapi.Session) { s.RuntimeAssociation, s.A2AAuthority = nil, "" }
	got, err := e.svc.LiveTarget(context.Background(), mainID)
	if err != nil || got.Actor != "kagent/"+kagent.ActorName(ksid) {
		t.Fatalf("%+v %v", got, err)
	}
}

// A Session that is not READY has no Actor to attach to yet.
func TestLiveTargetOfASessionThatIsNotReadyIsWaking(t *testing.T) {
	e, mainID, _ := liveEnv(t)
	e.srv.TamperSession = func(_ string, s *kagentapi.Session) { s.State = kagentapi.RuntimeState_RUNTIME_STATE_CREATING }
	if _, err := e.svc.LiveTarget(context.Background(), mainID); !errors.Is(err, liveactor.ErrWaking) {
		t.Fatalf("%v, want ErrWaking", err)
	}
}

func TestLiveTargetRefusesSessionsThatAreNotLive(t *testing.T) {
	e, _, _ := liveEnv(t)
	if _, err := e.svc.LiveTarget(context.Background(), "missing"); !errors.Is(err, liveactor.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// The same main session on an Agent that is not listed as live.
	off := newEnv(t)
	off.srv.Actors = true
	off.run()
	off.waitState(off.send("one"), dbstate.DeliveryCompleted)
	m, err := off.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := off.svc.LiveTarget(context.Background(), m.SessionID); !errors.Is(err, liveactor.ErrNotFound) {
		t.Fatalf("Agent not live: %v", err)
	}
}
