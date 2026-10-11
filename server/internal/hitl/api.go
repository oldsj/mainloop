package hitl

import (
	"context"
	"errors"

	"github.com/oldsj/mainloop/server/internal/api"
)

// API serves the HITL routes. Merge details are not served here yet; they stay on the 501 stub.
type API struct {
	Svc *Service
}

// NewAPI builds the API.
func NewAPI(svc *Service) *API { return &API{Svc: svc} }

func invalid(msg string) api.HTTPValidationError {
	return api.HTTPValidationError{Detail: &[]api.ValidationError{{
		Loc: []api.ValidationError_Loc_Item{}, Msg: msg, Type: "value_error",
	}}}
}

// ListSessionHITL implements api.StrictServerInterface.
func (a *API) ListSessionHITL(ctx context.Context, r api.ListSessionHITLRequestObject) (api.ListSessionHITLResponseObject, error) {
	ids, err := a.Svc.SessionRequests(ctx, r.SessionId)
	if errors.Is(err, ErrNotFound) {
		return api.ListSessionHITL404JSONResponse(api.HTTPError{Detail: "Session not found"}), nil
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
		return api.GetHITL404JSONResponse(api.HTTPError{Detail: "HITL request not found"}), nil
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
		return api.RespondHITL503JSONResponse(api.HTTPError{Detail: "Owner HITL writes are disabled"}), nil
	}
	if r.Body == nil {
		return api.RespondHITL422JSONResponse(invalid("the body is required")), nil
	}
	v, err := a.Svc.Respond(ctx, r.RequestId, *r.Body)
	switch {
	case errors.Is(err, ErrDisabled):
		return api.RespondHITL503JSONResponse(api.HTTPError{Detail: "Owner HITL writes are disabled"}), nil
	case errors.Is(err, ErrNotFound):
		return api.RespondHITL404JSONResponse(api.HTTPError{Detail: "HITL request not found"}), nil
	case errors.Is(err, ErrConflict):
		return api.RespondHITL409JSONResponse(api.HTTPError{Detail: invalidDetail(err, ErrConflict)}), nil
	case errors.Is(err, ErrInvalid):
		return api.RespondHITL422JSONResponse(invalid(invalidMessage(err))), nil
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
