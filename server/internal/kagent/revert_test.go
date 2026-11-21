package kagent_test

import (
	"testing"

	"connectrpc.com/connect"

	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	api "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

func TestRevertSession(t *testing.T) {
	t.Run("Given a crashed Session, when it is reverted, assert that it is READY with no operation and no longer reads as crashed", func(t *testing.T) {
		c, srv, _ := setup(t)
		id := crashedSession(t, c, srv)
		live, err := c.ObserveLiveness(ctx, id)
		if err != nil || !live.Terminal || !live.Recoverable {
			t.Fatalf("crashed liveness = %+v, %v, want Terminal and Recoverable", live, err)
		}
		got, err := c.RevertSession(ctx, id)
		if err != nil || got.GetState() != api.RuntimeState_RUNTIME_STATE_READY ||
			got.GetOperation() != api.RuntimeOperation_RUNTIME_OPERATION_NONE || kagent.CrashRecoverable(got) {
			t.Fatalf("reverted = %v, %v", got, err)
		}
	})

	t.Run("Given a crashed Session kagent cannot revert, when it is reverted, assert that the refusal is a FailedPrecondition SessionError", func(t *testing.T) {
		c, srv, _ := setup(t)
		srv.Revert = kagenttest.RevertUnrecoverable
		id := crashedSession(t, c, srv)
		_, err := c.RevertSession(ctx, id)
		if se := asType[*kagent.SessionError](t, err); se.Code != connect.CodeFailedPrecondition {
			t.Fatalf("code %v", se.Code)
		}
	})

	t.Run("Given a FAILED Session with no revert operation, when its liveness is read, assert that it is Terminal and not Recoverable", func(t *testing.T) {
		c, srv, _ := setup(t)
		id := crashedSession(t, c, srv)
		srv.SetRuntime(id, api.RuntimeState_RUNTIME_STATE_FAILED, api.RuntimeOperation_RUNTIME_OPERATION_NONE)
		live, err := c.ObserveLiveness(ctx, id)
		if err != nil || !live.Terminal || live.Recoverable {
			t.Fatalf("liveness = %+v, %v", live, err)
		}
	})
}

func crashedSession(t *testing.T, c *kagent.Client, srv *kagenttest.Server) string {
	t.Helper()
	s, err := c.CreateSession(ctx, &api.CreateSessionRequest{RequestId: "req-crash"})
	if err != nil {
		t.Fatal(err)
	}
	srv.Crash(s.GetId())
	return s.GetId()
}
