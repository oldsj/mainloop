package health

import (
	"context"
	"errors"

	"github.com/oldsj/mainloop/server/internal/api"
)

// DataAPI serves the orchestration-health and smoke-observation routes. (GET /health, the liveness
// check, is the app's own.) The type name is unique because the app embeds every domain's API in
// one struct.
type DataAPI struct {
	Svc *Service
}

// NewAPI builds the API.
func NewAPI(svc *Service) *DataAPI { return &DataAPI{Svc: svc} }

const defaultHours = 24

func invalid(msg string) api.HTTPValidationError {
	return api.HTTPValidationError{Detail: &[]api.ValidationError{{
		Loc: []api.ValidationError_Loc_Item{}, Msg: msg, Type: "value_error",
	}}}
}

// hours applies the contract's default and bounds. The request validator already rejects values
// outside them; this keeps the handler safe when called without it.
func hours(p *int) (int, bool) {
	if p == nil {
		return defaultHours, true
	}
	return *p, *p >= 1 && *p <= 8760
}

// OrchestrationHealth implements api.StrictServerInterface.
func (a *DataAPI) OrchestrationHealth(ctx context.Context, r api.OrchestrationHealthRequestObject) (api.OrchestrationHealthResponseObject, error) {
	h, ok := hours(r.Params.Hours)
	if !ok {
		return api.OrchestrationHealth422JSONResponse(invalid("hours must be between 1 and 8760")), nil
	}
	view, err := a.Svc.View(ctx, Filter{
		ProjectID: r.Params.ProjectId, TaskID: r.Params.TaskId, SessionID: r.Params.SessionId,
		AttemptID: r.Params.AttemptId, Provider: r.Params.Provider, Hours: h,
	})
	if errors.Is(err, ErrNotFound) {
		return api.OrchestrationHealth404JSONResponse(api.HTTPError{Detail: "Not found"}), nil
	}
	if err != nil {
		return nil, err
	}
	return api.OrchestrationHealth200JSONResponse(view), nil
}

// HealthEvents implements api.StrictServerInterface.
func (a *DataAPI) HealthEvents(ctx context.Context, r api.HealthEventsRequestObject) (api.HealthEventsResponseObject, error) {
	h, ok := hours(r.Params.Hours)
	if !ok || r.Params.SessionId == "" {
		return api.HealthEvents422JSONResponse(invalid("session_id is required and hours must be between 1 and 8760")), nil
	}
	out, err := a.Svc.Events(ctx, r.Params.SessionId, h)
	if errors.Is(err, ErrNotFound) {
		return api.HealthEvents404JSONResponse(api.HTTPError{Detail: "Session not found"}), nil
	}
	if err != nil {
		return nil, err
	}
	return api.HealthEvents200JSONResponse(out), nil
}

// GetProjectSmokeObservations implements api.StrictServerInterface.
func (a *DataAPI) GetProjectSmokeObservations(ctx context.Context, r api.GetProjectSmokeObservationsRequestObject) (api.GetProjectSmokeObservationsResponseObject, error) {
	out, err := a.Svc.Smoke(ctx, r.ProjectId, r.Params.Branch)
	if errors.Is(err, ErrNotFound) {
		return api.GetProjectSmokeObservations404JSONResponse(api.HTTPError{Detail: "Project not found"}), nil
	}
	if err != nil {
		return nil, err
	}
	return api.GetProjectSmokeObservations200JSONResponse(out), nil
}
