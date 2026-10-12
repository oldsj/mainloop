package sessions_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

func TestStandaloneCompletedSessionAcceptsFollowup(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	sid := e.newSession("first turn")
	stop := e.run()
	e.waitState(e.firstDelivery(sid), dbstate.DeliveryCompleted)
	stop()
	// Completion is activity, and standalone sessions have no stopped task attempt.
	if _, err := e.pool.Exec(context.Background(), `UPDATE sessions SET status='completed' WHERE id=$1`, sid); err != nil {
		t.Fatal(err)
	}
	mid := uuid.NewString()
	if _, err := e.svc.SendTo(context.Background(), sid, mid, "follow up"); err != nil {
		t.Fatal(err)
	}
	e.run()
	e.waitState(mid, dbstate.DeliveryCompleted)
	if n := e.srv.Count("SendStreamingMessage"); n != 2 {
		t.Fatalf("native sends = %d, want first turn and follow-up", n)
	}
}
