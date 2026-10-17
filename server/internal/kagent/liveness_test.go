package kagent_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	api "github.com/oldsj/mainloop/server/third_party/kagentapi"
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
	session := marshal(t, &api.Session{Id: "s-1", ContextId: "s-1", State: api.RuntimeState_RUNTIME_STATE_SUSPENDED})
	live, err := rawSessionServer(t, session).ObserveLiveness(ctx, "s-1")
	if err != nil || !live.Exists || live.Terminal {
		t.Fatalf("suspended: live = %+v, err = %v, want Exists and not Terminal", live, err)
	}
}
