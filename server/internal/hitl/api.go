package hitl

import (
	"context"
	"errors"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
)

// API serves the HITL routes. The merge package serves the paged merge details route.
type API struct {
	Svc *Service
}

// NewAPI builds the API.
func NewAPI(svc *Service) *API { return &API{Svc: svc} }

// ListSessionHITL implements api.StrictServerInterface.
func (a *API) ListSessionHITL(ctx context.Context, r api.ListSessionHITLRequestObject) (api.ListSessionHITLResponseObject, error) {
	ids, err := a.Svc.SessionRequests(ctx, r.SessionId)
	if errors.Is(err, ErrNotFound) {
		return nil, apierr.NotFound("Session not found")
	}
	if err != nil {
		return nil, err
	}
	return api.ListSessionHITL200JSONResponse(ids), nil
}

// GetHITL implements api.StrictServerInterface.
func (a *API) GetHITL(ctx context.Context, r api.GetHITLRequestObject) (api.GetHITLResponseObject, error) {
	v, err := a.Svc.View(ctx, r.RequestId)
	if errors.Is(err, ErrNotFound) {
		return nil, apierr.NotFound("HITL request not found")
	}
	if err != nil {
		return nil, err
	}
	return api.GetHITL200JSONResponse(v), nil
}

// RespondHITL implements api.StrictServerInterface. With the flag off it answers 503 before it
// looks at the request.
func (a *API) RespondHITL(ctx context.Context, r api.RespondHITLRequestObject) (api.RespondHITLResponseObject, error) {
	if !a.Svc.WritesEnabled {
		return nil, apierr.Unavailable("Owner HITL writes are disabled")
	}
	if r.Body == nil {
		return nil, apierr.Invalid("the body is required")
	}
	v, err := a.Svc.Respond(ctx, r.RequestId, *r.Body)
	switch {
	case errors.Is(err, ErrDisabled):
		return nil, apierr.Unavailable("Owner HITL writes are disabled")
	case errors.Is(err, ErrNotFound):
		return nil, apierr.NotFound("HITL request not found")
	case errors.Is(err, ErrConflict):
		return nil, apierr.Conflict(invalidDetail(err, ErrConflict))
	case errors.Is(err, ErrInvalid):
		return nil, apierr.Invalid(invalidMessage(err))
	case err != nil:
		return nil, err
	}
	return api.RespondHITL200JSONResponse(v), nil
}

func invalidDetail(err, sentinel error) string {
	msg := err.Error()
	if p := sentinel.Error() + ": "; len(msg) > len(p) && msg[:len(p)] == p {
		return msg[len(p):]
	}
	return msg
}

// GetHITLObserverStatus implements api.StrictServerInterface.
func (a *API) GetHITLObserverStatus(ctx context.Context, _ api.GetHITLObserverStatusRequestObject) (api.GetHITLObserverStatusResponseObject, error) {
	st, err := a.Svc.Status(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetHITLObserverStatus200JSONResponse(st), nil
}
