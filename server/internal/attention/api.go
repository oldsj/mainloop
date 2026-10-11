package attention

import (
	"context"
	"errors"

	"github.com/oldsj/mainloop/server/internal/api"
)

// AttentionAPI serves the attention, activity and queue routes. The type name is unique because
// the app embeds every domain's API in one struct.
type AttentionAPI struct {
	Svc *Service
}

// NewAPI builds the API.
func NewAPI(svc *Service) *AttentionAPI { return &AttentionAPI{Svc: svc} }

func invalid(msg string) api.HTTPValidationError {
	return api.HTTPValidationError{Detail: &[]api.ValidationError{{
		Loc: []api.ValidationError_Loc_Item{}, Msg: msg, Type: "value_error",
	}}}
}

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
		return api.RespondToQueueItem422JSONResponse(invalid("the body is required")), nil
	}
	err := a.Svc.Respond(ctx, r.ItemId, *r.Body)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.RespondToQueueItem404JSONResponse(api.HTTPError{Detail: "Queue item not found"}), nil
	case errors.Is(err, ErrConflict):
		return api.RespondToQueueItem409JSONResponse(api.HTTPError{Detail: message(err, ErrConflict)}), nil
	case errors.Is(err, ErrInvalid):
		return api.RespondToQueueItem422JSONResponse(invalid(message(err, ErrInvalid))), nil
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
		return api.DismissQueueDiagnostic404JSONResponse(api.HTTPError{Detail: "Queue item not found"}), nil
	case errors.Is(err, ErrConflict):
		return api.DismissQueueDiagnostic409JSONResponse(api.HTTPError{Detail: message(err, ErrConflict)}), nil
	case err != nil:
		return nil, err
	}
	return api.DismissQueueDiagnostic200JSONResponse(api.StatusOk{Status: api.StatusOkStatusOk}), nil
}
