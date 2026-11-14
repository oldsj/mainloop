package projects

import (
	"context"
	"errors"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/ci"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

func toVerification(v ci.Verification) api.ProjectVerification {
	out := api.ProjectVerification{
		Mode: api.VerificationMode(v.Mode), EffectiveMode: api.EffectiveVerificationMode(v.Effective),
		Source: api.ProjectVerificationSource(v.Source),
	}
	if v.DetectError != "" {
		out.DetectError = &v.DetectError
	}
	if d := v.Detection; d != nil {
		det := api.CIDetection{
			Usable: d.Usable, Branch: d.Branch, Checks: d.Checks, Sources: d.Sources, CheckedAt: d.CheckedAt,
		}
		if det.Sources == nil {
			det.Sources = []string{}
		}
		if d.SHA != "" {
			det.Sha = &d.SHA
		}
		out.Detection = &det
	}
	return out
}

// GetProjectVerification implements api.StrictServerInterface: the owner's read of where the
// project's heavy checks run. A process without a GitHub App answers 404, as it has no CI reader.
func (a *ProjectsAPI) GetProjectVerification(ctx context.Context, r api.GetProjectVerificationRequestObject) (api.GetProjectVerificationResponseObject, error) {
	if a.CI == nil {
		return nil, apierr.NotFound("Project not found")
	}
	refresh := r.Params.Refresh != nil && *r.Params.Refresh
	v, err := a.CI.Verification(ctx, a.Svc.OwnerID, r.ProjectId, refresh)
	if errors.Is(err, ci.ErrProjectNotFound) {
		return nil, apierr.NotFound("Project not found")
	}
	if err != nil {
		return nil, err
	}
	return api.GetProjectVerification200JSONResponse(toVerification(v)), nil
}

// UpdateProjectVerification implements api.StrictServerInterface: owner-only, Mainloop-side
// configuration, never read from the project's own repository.
func (a *ProjectsAPI) UpdateProjectVerification(ctx context.Context, r api.UpdateProjectVerificationRequestObject) (api.UpdateProjectVerificationResponseObject, error) {
	if a.CI == nil {
		return nil, apierr.NotFound("Project not found")
	}
	v, err := a.CI.SetVerificationMode(ctx, a.Svc.OwnerID, r.ProjectId, dbstate.VerificationMode(r.Body.Mode))
	switch {
	case errors.Is(err, ci.ErrProjectNotFound):
		return nil, apierr.NotFound("Project not found")
	case errors.Is(err, ci.ErrInvalidMode):
		return nil, apierr.Invalid("Unknown verification mode")
	case err != nil:
		return nil, err
	}
	return api.UpdateProjectVerification200JSONResponse(toVerification(v)), nil
}
