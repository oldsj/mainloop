package kagent

import (
	"context"

	api "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

// CreateCheckpoint creates a Checkpoint at a Session's terminal task. Calling again with the same
// RequestID is safe: kagent dedupes on it and returns the Checkpoint it already made, the same way
// CreateSession does (see [Client.CreateSession]).
// CreateCheckpoint's FailedPrecondition reasons (kagent #94, ErrorInfo domain kagent.dev). Only a
// READY checkpoint is acknowledged; Unavailable is reserved for an attempt left CREATING, which the
// same request id resumes.
const (
	// ReasonCheckpointSnapshotPending: the snapshot is not ready yet. Retry the same request id
	// and head task.
	ReasonCheckpointSnapshotPending = "KAGENT_CHECKPOINT_SNAPSHOT_PENDING"
	// ReasonCheckpointConversationAdvanced: the conversation moved past the expected head task.
	// Choose a new boundary.
	ReasonCheckpointConversationAdvanced = "KAGENT_CHECKPOINT_CONVERSATION_ADVANCED"
	// ReasonCheckpointRuntimeNotSuspended: the Actor was running, so there was no stable snapshot.
	// The request is recorded failed; a new attempt needs a new request id.
	ReasonCheckpointRuntimeNotSuspended = "KAGENT_CHECKPOINT_RUNTIME_NOT_SUSPENDED"
	// ReasonCheckpointFailed: the snapshot copy failed, or a replay of a failed request id. A new
	// attempt needs a new request id.
	ReasonCheckpointFailed = "KAGENT_CHECKPOINT_FAILED"
)

func (c *Client) CreateCheckpoint(ctx context.Context, req *api.CreateCheckpointRequest) (*api.Checkpoint, error) {
	const op = "CheckpointService CreateCheckpoint"
	resp, err := unary(ctx, c, op, c.checkpointCl.CreateCheckpoint, req)
	if err != nil {
		return nil, err
	}
	cp := resp.GetCheckpoint()
	if cp.GetId() == "" || cp.GetSessionId() != req.GetSessionId() {
		return nil, &OutcomeUnknown{Op: op, Detail: "reply differs from the create contract"}
	}
	return cp, nil
}

// GetCheckpoint reads one Checkpoint.
func (c *Client) GetCheckpoint(ctx context.Context, id string) (*api.Checkpoint, error) {
	const op = "CheckpointService GetCheckpoint"
	if id == "" {
		return nil, emptyArg(op, "checkpoint id")
	}
	resp, err := unary(ctx, c, op, c.checkpointCl.GetCheckpoint, &api.GetCheckpointRequest{CheckpointId: id})
	if err != nil {
		return nil, err
	}
	cp := resp.GetCheckpoint()
	if cp.GetId() != id {
		return nil, &OutcomeUnknown{Op: op, Detail: "reply is for a different checkpoint"}
	}
	return cp, nil
}

// ListCheckpointsPage reads one page of a Session's checkpoints. An empty next token means the
// last page.
func (c *Client) ListCheckpointsPage(
	ctx context.Context, sessionID, token string, limit int,
) (checkpoints []*api.Checkpoint, next string, err error) {
	const op = "CheckpointService ListCheckpoints"
	if sessionID == "" {
		return nil, "", emptyArg(op, "session id")
	}
	limit = min(max(limit, 1), maxPageSize)
	resp, err := unary(ctx, c, op, c.checkpointCl.ListCheckpoints, &api.ListCheckpointsRequest{
		SessionId: sessionID,
		Page:      &api.PageRequest{Limit: int32(limit), PageToken: token}, //nolint:gosec // limit is 1..100
	})
	if err != nil {
		return nil, "", err
	}
	return resp.GetCheckpoints(), resp.GetPage().GetNextPageToken(), nil
}

// ListCheckpoints reads every Checkpoint of a Session.
func (c *Client) ListCheckpoints(ctx context.Context, sessionID string) ([]*api.Checkpoint, error) {
	return paginate("CheckpointService ListCheckpoints", func(token string) ([]*api.Checkpoint, string, error) {
		return c.ListCheckpointsPage(ctx, sessionID, token, maxPageSize)
	})
}

// DeleteCheckpoint deletes a Checkpoint. Deleting one that is already gone is not an error: the
// caller gets back the SessionError a repeat delete gets, which the caller treats as settled.
func (c *Client) DeleteCheckpoint(ctx context.Context, id string) error {
	const op = "CheckpointService DeleteCheckpoint"
	if id == "" {
		return emptyArg(op, "checkpoint id")
	}
	_, err := unary(ctx, c, op, c.checkpointCl.DeleteCheckpoint, &api.DeleteCheckpointRequest{CheckpointId: id})
	return err
}

// ForkSession forks a Session from a Checkpoint. Calling again with the same RequestID is safe:
// kagent dedupes on it and returns the Session it already forked, the same way CreateSession does.
func (c *Client) ForkSession(ctx context.Context, req *api.ForkSessionRequest) (*api.Session, error) {
	const op = "CheckpointService ForkSession"
	resp, err := unary(ctx, c, op, c.checkpointCl.ForkSession, req)
	if err != nil {
		return nil, err
	}
	s := resp.GetSession()
	if s.GetId() == "" || s.GetContextId() != s.GetId() {
		return nil, &OutcomeUnknown{Op: op, Detail: "reply differs from the fork contract"}
	}
	return s, nil
}
