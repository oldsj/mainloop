package sessions

import (
	"context"
	"errors"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
)

func toAPICheckpoint(v CheckpointView) api.Checkpoint {
	return api.Checkpoint{
		Id: v.ID, SessionId: v.SessionID, HeadTaskId: v.HeadTaskID, Name: v.Name,
		State: api.CheckpointState(v.State),
	}
}

func checkpointError(err error) error {
	var kerr *KagentError
	switch {
	case errors.Is(err, ErrNotFound):
		return apierr.NotFound("Checkpoint not found")
	case errors.Is(err, ErrUnavailable):
		return apierr.Unavailable("The agent runtime is not configured")
	case errors.As(err, &kerr):
		return apierr.BadGateway("kagent error: " + safeDetail(kerr.Err.Error()))
	default:
		return err
	}
}

// ListSessionCheckpoints implements api.StrictServerInterface.
func (a *SessionsAPI) ListSessionCheckpoints(
	ctx context.Context, r api.ListSessionCheckpointsRequestObject,
) (api.ListSessionCheckpointsResponseObject, error) {
	views, err := a.Svc.ListCheckpoints(ctx, r.SessionId)
	if err != nil {
		return nil, checkpointError(err)
	}
	out := make(api.ListSessionCheckpoints200JSONResponse, len(views))
	for i, v := range views {
		out[i] = toAPICheckpoint(v)
	}
	return out, nil
}

// CreateSessionCheckpoint implements api.StrictServerInterface.
func (a *SessionsAPI) CreateSessionCheckpoint(
	ctx context.Context, r api.CreateSessionCheckpointRequestObject,
) (api.CreateSessionCheckpointResponseObject, error) {
	if r.Body == nil {
		return nil, apierr.Invalid("the body is required")
	}
	res, err := a.Svc.CreateCheckpoint(ctx, r.SessionId, r.Body.RequestId.String())
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, apierr.NotFound("Session not found")
	case errors.Is(err, ErrConflict):
		return nil, apierr.Conflict(reason(err, ErrConflict))
	case errors.Is(err, ErrInvalid):
		return nil, apierr.Invalid(reason(err, ErrInvalid))
	case err != nil:
		return nil, checkpointError(err)
	}
	return api.CreateSessionCheckpoint200JSONResponse(api.Checkpoint{
		Id: res.ID, SessionId: res.SessionID, HeadTaskId: res.HeadTaskID, Name: res.Name,
		State: api.CheckpointState(res.State),
	}), nil
}

// GetCheckpoint implements api.StrictServerInterface.
func (a *SessionsAPI) GetCheckpoint(ctx context.Context, r api.GetCheckpointRequestObject) (api.GetCheckpointResponseObject, error) {
	v, err := a.Svc.GetCheckpoint(ctx, r.CheckpointId)
	if err != nil {
		return nil, checkpointError(err)
	}
	return api.GetCheckpoint200JSONResponse(toAPICheckpoint(v)), nil
}

// DeleteCheckpoint implements api.StrictServerInterface.
func (a *SessionsAPI) DeleteCheckpoint(ctx context.Context, r api.DeleteCheckpointRequestObject) (api.DeleteCheckpointResponseObject, error) {
	err := a.Svc.DeleteCheckpoint(ctx, r.CheckpointId)
	switch {
	case errors.Is(err, ErrConflict):
		return nil, apierr.Conflict(reason(err, ErrConflict))
	case err != nil:
		return nil, checkpointError(err)
	}
	return api.DeleteCheckpoint200JSONResponse(api.StatusOk{Status: api.StatusOkStatusOk}), nil
}

// ForkCheckpoint implements api.StrictServerInterface.
func (a *SessionsAPI) ForkCheckpoint(ctx context.Context, r api.ForkCheckpointRequestObject) (api.ForkCheckpointResponseObject, error) {
	if r.Body == nil {
		return nil, apierr.Invalid("the body is required")
	}
	fresh := r.Body.FreshConversation != nil && *r.Body.FreshConversation
	title := ""
	if r.Body.Title != nil {
		title = *r.Body.Title
	}
	res, err := a.Svc.Fork(ctx, r.CheckpointId, r.Body.RequestId.String(), fresh, title)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, apierr.NotFound("Checkpoint not found")
	case errors.Is(err, ErrConflict):
		return nil, apierr.Conflict(reason(err, ErrConflict))
	case errors.Is(err, ErrInvalid):
		return nil, apierr.Invalid(reason(err, ErrInvalid))
	case err != nil:
		return nil, checkpointError(err)
	}
	return api.ForkCheckpoint200JSONResponse(api.CheckpointForkResponse{
		NewSessionId: res.NewSessionID, CheckpointId: r.CheckpointId,
		RequestedAt: res.RequestedAt, AcceptedAt: res.AcceptedAt, ReadyAt: res.ReadyAt,
	}), nil
}
