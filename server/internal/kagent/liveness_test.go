package kagent_test

import (
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/a2aproject/a2a-go/v2/a2a"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	api "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

// kagenttest's fake SessionService always stamps a fresh updated_at on create, so it cannot
// produce the wire shape this guards against; these use the raw byte-level server wire_test.go
// already builds for exactly that reason.

func TestObserveLivenessOfAMissingUpdatedAtIsTheZeroTimeNotTheEpoch(t *testing.T) {
	session := marshal(t, &api.Session{Id: "s-1", ContextId: "s-1", State: api.RuntimeState_RUNTIME_STATE_READY})
	live, err := rawSessionServer(t, session).ObserveLiveness(ctx, "s-1")
	if err != nil || !live.Exists || live.Terminal {
		t.Fatalf("live = %+v, err = %v", live, err)
	}
	if !live.LastActivity.IsZero() {
		t.Fatalf("LastActivity = %v, want the zero time for a Session with no updated_at", live.LastActivity)
	}
}

func TestObserveLivenessReadsTheUpdatedAtWhenSet(t *testing.T) {
	when := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	session := marshal(t, &api.Session{
		Id: "s-1", ContextId: "s-1", State: api.RuntimeState_RUNTIME_STATE_READY, UpdatedAt: timestamppb.New(when),
	})
	live, err := rawSessionServer(t, session).ObserveLiveness(ctx, "s-1")
	if err != nil || !live.LastActivity.Equal(when) {
		t.Fatalf("live = %+v, err = %v, want LastActivity = %v", live, err, when)
	}
}

func TestObserveLivenessReportsTerminalRuntimeStates(t *testing.T) {
	terminal := map[string]api.RuntimeState{
		"failed": api.RuntimeState_RUNTIME_STATE_FAILED, "deleting": api.RuntimeState_RUNTIME_STATE_DELETING,
		"deleted": api.RuntimeState_RUNTIME_STATE_DELETED,
	}
	for name, state := range terminal {
		t.Run(name, func(t *testing.T) {
			session := marshal(t, &api.Session{Id: "s-1", ContextId: "s-1", State: state})
			live, err := rawSessionServer(t, session).ObserveLiveness(ctx, "s-1")
			if err != nil || !live.Exists || !live.Terminal {
				t.Fatalf("live = %+v, err = %v", live, err)
			}
		})
	}
	// A sleeping Session is READY with an association that is not current; an awake one carries a
	// current association. Without an association kagent said nothing, which is not asleep.
	session := marshal(t, &api.Session{Id: "s-1", ContextId: "s-1", State: api.RuntimeState_RUNTIME_STATE_READY})
	live, err := rawSessionServer(t, session).ObserveLiveness(ctx, "s-1")
	if err != nil || !live.Exists || live.Terminal || live.Asleep {
		t.Fatalf("no association: live = %+v, err = %v, want Exists and not Asleep", live, err)
	}
	session = marshal(t, &api.Session{
		Id: "s-1", ContextId: "s-1", State: api.RuntimeState_RUNTIME_STATE_READY,
		RuntimeAssociation: &api.RuntimeAssociation{GenerationId: "g", Atespace: "ate", ActorName: "session-s-1", ActorUid: "u", Phase: "active"},
	})
	live, err = rawSessionServer(t, session).ObserveLiveness(ctx, "s-1")
	if err != nil || !live.Exists || live.Terminal || !live.Asleep {
		t.Fatalf("asleep: live = %+v, err = %v, want Exists, Asleep and not Terminal", live, err)
	}
	session = marshal(t, &api.Session{
		Id: "s-1", ContextId: "s-1", State: api.RuntimeState_RUNTIME_STATE_READY,
		RuntimeAssociation: &api.RuntimeAssociation{GenerationId: "g", Atespace: "ate", ActorName: "session-s-1", ActorUid: "u", CurrentActive: true},
	})
	live, err = rawSessionServer(t, session).ObserveLiveness(ctx, "s-1")
	if err != nil || !live.Exists || live.Terminal || live.Asleep {
		t.Fatalf("awake: live = %+v, err = %v, want Exists and not Asleep", live, err)
	}
}

// A forced delete carries force on the wire; an ordinary one is refused while a turn runs.
func TestForceDeleteSessionTerminatesARunningTurn(t *testing.T) {
	srv := kagenttest.New()
	t.Cleanup(srv.Close)
	srv.AddAgent("kagent", "a")
	c, err := kagent.New(kagent.Config{GatewayURL: srv.URL, UserID: "mainloop"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.CreateSession(ctx, &api.CreateSessionRequest{Agent: &api.ResourceReference{Namespace: "kagent", Name: "a"}, RequestId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	srv.AddTask(&a2a.Task{ID: "t1", ContextID: s.GetId(), Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
	var se *kagent.SessionError
	if _, err := c.DeleteSession(ctx, s.GetId()); !errors.As(err, &se) || se.Code != connect.CodeFailedPrecondition {
		t.Fatalf("delete with a running turn: %v, want FailedPrecondition", err)
	}
	if _, err := c.ForceDeleteSession(ctx, s.GetId()); err != nil {
		t.Fatal(err)
	}
	if got := srv.ForcedDeletes(); len(got) != 1 || got[0] != s.GetId() {
		t.Fatalf("forced deletes %v", got)
	}
	if _, ok := srv.Session(s.GetId()); ok {
		t.Fatal("session still stored")
	}
}

// Deleting is set only for RUNTIME_STATE_DELETING, never for the other terminal states: a caller
// deciding whether anything in the session could still be running must be able to tell a session
// still on its way out from one that has already settled failed or deleted.
func TestObserveLivenessSetsDeletingOnlyForTheDeletingState(t *testing.T) {
	session := marshal(t, &api.Session{Id: "s-1", ContextId: "s-1", State: api.RuntimeState_RUNTIME_STATE_DELETING})
	live, err := rawSessionServer(t, session).ObserveLiveness(ctx, "s-1")
	if err != nil || !live.Terminal || !live.Deleting {
		t.Fatalf("deleting: live = %+v, err = %v, want Terminal and Deleting", live, err)
	}
	for _, state := range []api.RuntimeState{api.RuntimeState_RUNTIME_STATE_FAILED, api.RuntimeState_RUNTIME_STATE_DELETED} {
		session := marshal(t, &api.Session{Id: "s-1", ContextId: "s-1", State: state})
		live, err := rawSessionServer(t, session).ObserveLiveness(ctx, "s-1")
		if err != nil || !live.Terminal || live.Deleting {
			t.Fatalf("%v: live = %+v, err = %v, want Terminal and not Deleting", state, live, err)
		}
	}
}

// kagent records the Actor's host as its A2A authority; ActorOf reads the atespace and name from it.
func TestActorOfReadsTheA2AAuthority(t *testing.T) {
	for authority, want := range map[string][2]string{
		"session-abc.team-a.actors.resources.substrate.ate.dev": {"team-a", "session-abc"},
		"": {},
		"session-abc.actors.resources.substrate.ate.dev":              {},
		"session-abc.team-a.extra.actors.resources.substrate.ate.dev": {},
		"session-abc.team-a.example.test":                             {},
	} {
		atespace, name, ok := kagent.ActorOf(&api.Session{A2AAuthority: authority})
		if ok != (want[0] != "") || atespace != want[0] || name != want[1] {
			t.Errorf("%q: %q %q %v, want %q", authority, atespace, name, ok, want)
		}
	}
}
