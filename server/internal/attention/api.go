package attention

import (
	"context"
	"errors"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
)

// AttentionAPI serves the attention, activity and queue routes. The type name is unique because
// the app embeds every domain's API in one struct.
type AttentionAPI struct {
	Svc *Service
}

// NewAPI builds the API.
func NewAPI(svc *Service) *AttentionAPI { return &AttentionAPI{Svc: svc} }

// message is an error's text without its sentinel prefix.
func message(err, sentinel error) string {
	msg, prefix := err.Error(), sentinel.Error()+": "
	if len(msg) > len(prefix) && msg[:len(prefix)] == prefix {
		return msg[len(prefix):]
	}
	return msg
}

// GetAttention implements api.StrictServerInterface.
func (a *AttentionAPI) GetAttention(ctx context.Context, _ api.GetAttentionRequestObject) (api.GetAttentionResponseObject, error) {
	snap, err := a.Svc.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetAttention200JSONResponse(snap), nil
}

// ListActivity implements api.StrictServerInterface.
func (a *AttentionAPI) ListActivity(ctx context.Context, _ api.ListActivityRequestObject) (api.ListActivityResponseObject, error) {
	items, err := a.Svc.Activity(ctx)
	if err != nil {
		return nil, err
	}
	return api.ListActivity200JSONResponse(items), nil
}

// RespondToQueueItem implements api.StrictServerInterface.
func (a *AttentionAPI) RespondToQueueItem(ctx context.Context, r api.RespondToQueueItemRequestObject) (api.RespondToQueueItemResponseObject, error) {
	if r.Body == nil {
		return nil, apierr.Invalid("the body is required")
	}
	err := a.Svc.Respond(ctx, r.ItemId, *r.Body)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, apierr.NotFound("Queue item not found")
	case errors.Is(err, ErrConflict):
		return nil, apierr.Conflict(message(err, ErrConflict))
	case errors.Is(err, ErrInvalid):
		return nil, apierr.Invalid(message(err, ErrInvalid))
	case err != nil:
		return nil, err
	}
	return api.RespondToQueueItem200JSONResponse(api.QueueRespondResponse{
		Status: api.QueueRespondResponseStatusOk, Message: api.ResponseSent,
	}), nil
}

// DismissQueueDiagnostic implements api.StrictServerInterface.
func (a *AttentionAPI) DismissQueueDiagnostic(ctx context.Context, r api.DismissQueueDiagnosticRequestObject) (api.DismissQueueDiagnosticResponseObject, error) {
	err := a.Svc.Dismiss(ctx, r.ItemId)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, apierr.NotFound("Queue item not found")
	case errors.Is(err, ErrConflict):
		return nil, apierr.Conflict(message(err, ErrConflict))
	case err != nil:
		return nil, err
	}
	return api.DismissQueueDiagnostic200JSONResponse(api.StatusOk{Status: api.StatusOkStatusOk}), nil
}
