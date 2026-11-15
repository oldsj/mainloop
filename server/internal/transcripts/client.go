package transcripts

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/oldsj/mainloop/server/internal/kagent"
)

// FromClient adapts the shared kagent client.
func FromClient(c *kagent.Client) Kagent { return clientAdapter{c} }

type clientAdapter struct{ *kagent.Client }

// AgentExists reads the Agent. Only kagent's own not-found says it is missing; any other failure is
// returned, since it proves nothing.
func (a clientAdapter) AgentExists(ctx context.Context, agent kagent.AgentRef) (bool, error) {
	_, err := a.GetAgent(ctx, agent)
	var se *kagent.SessionError
	if errors.As(err, &se) && se.Code == connect.CodeNotFound && se.Wire {
		return false, nil
	}
	return err == nil, err
}
