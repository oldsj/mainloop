// Package environments is the environment registry: statically validated OCI images, their
// immutable versions, per-project grants and the per-project selection.
package environments

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// EnvironmentsAPI serves the environment and project-selection routes.
type EnvironmentsAPI struct {
	Service *Service
	// OwnerID is the one owner this instance serves.
	OwnerID string
}

// ListEnvironments implements the contract operation.
func (a *EnvironmentsAPI) ListEnvironments(ctx context.Context, _ api.ListEnvironmentsRequestObject) (api.ListEnvironmentsResponseObject, error) {
	out, err := a.Service.List(ctx, a.OwnerID)
	return api.ListEnvironments200JSONResponse(out), err
}

// RegisterEnvironment implements the contract operation.
func (a *EnvironmentsAPI) RegisterEnvironment(ctx context.Context, req api.RegisterEnvironmentRequestObject) (api.RegisterEnvironmentResponseObject, error) {
	out, err := a.Service.Register(ctx, a.OwnerID, *req.Body)
	if err != nil {
		return nil, err
	}
	return api.RegisterEnvironment201JSONResponse(out), nil
}

// GetEnvironment implements the contract operation.
func (a *EnvironmentsAPI) GetEnvironment(ctx context.Context, req api.GetEnvironmentRequestObject) (api.GetEnvironmentResponseObject, error) {
	out, err := a.Service.Get(ctx, a.OwnerID, req.EnvironmentId)
	if err != nil {
		return nil, err
	}
	return api.GetEnvironment200JSONResponse(out), nil
}

// UpdateEnvironment implements the contract operation.
func (a *EnvironmentsAPI) UpdateEnvironment(ctx context.Context, req api.UpdateEnvironmentRequestObject) (api.UpdateEnvironmentResponseObject, error) {
	out, err := a.Service.SetFollow(ctx, a.OwnerID, req.EnvironmentId, req.Body.FollowWatchedTag)
	if err != nil {
		return nil, err
	}
	return api.UpdateEnvironment200JSONResponse(out), nil
}

// ListEnvironmentVersions implements the contract operation.
func (a *EnvironmentsAPI) ListEnvironmentVersions(ctx context.Context, req api.ListEnvironmentVersionsRequestObject) (api.ListEnvironmentVersionsResponseObject, error) {
	out, err := a.Service.Versions(ctx, a.OwnerID, req.EnvironmentId)
	if err != nil {
		return nil, err
	}
	return api.ListEnvironmentVersions200JSONResponse(out), nil
}

// GetEnvironmentVersion implements the contract operation.
func (a *EnvironmentsAPI) GetEnvironmentVersion(ctx context.Context, req api.GetEnvironmentVersionRequestObject) (api.GetEnvironmentVersionResponseObject, error) {
	out, err := a.Service.Version(ctx, a.OwnerID, req.EnvironmentId, req.VersionId)
	if err != nil {
		return nil, err
	}
	return api.GetEnvironmentVersion200JSONResponse(out), nil
}

// RefreshEnvironment implements the contract operation.
func (a *EnvironmentsAPI) RefreshEnvironment(ctx context.Context, req api.RefreshEnvironmentRequestObject) (api.RefreshEnvironmentResponseObject, error) {
	out, err := a.Service.Refresh(ctx, a.OwnerID, req.EnvironmentId, req.Body.VersionId)
	if err != nil {
		return nil, err
	}
	return api.RefreshEnvironment201JSONResponse(out), nil
}

// AcceptEnvironmentDefault implements the contract operation.
func (a *EnvironmentsAPI) AcceptEnvironmentDefault(ctx context.Context, req api.AcceptEnvironmentDefaultRequestObject) (api.AcceptEnvironmentDefaultResponseObject, error) {
	out, err := a.Service.SetDefault(ctx, a.OwnerID, req.EnvironmentId, req.Body.VersionId)
	if err != nil {
		return nil, err
	}
	return api.AcceptEnvironmentDefault200JSONResponse(out), nil
}

// GrantEnvironmentToProject implements the contract operation.
func (a *EnvironmentsAPI) GrantEnvironmentToProject(ctx context.Context, req api.GrantEnvironmentToProjectRequestObject) (api.GrantEnvironmentToProjectResponseObject, error) {
	out, err := a.Service.Grant(ctx, a.OwnerID, req.EnvironmentId, req.ProjectId, dbstate.GrantPermission(req.Body.Permission))
	if err != nil {
		return nil, err
	}
	return api.GrantEnvironmentToProject200JSONResponse(out), nil
}

// RevokeEnvironmentFromProject implements the contract operation.
func (a *EnvironmentsAPI) RevokeEnvironmentFromProject(ctx context.Context, req api.RevokeEnvironmentFromProjectRequestObject) (api.RevokeEnvironmentFromProjectResponseObject, error) {
	if err := a.Service.Revoke(ctx, a.OwnerID, req.EnvironmentId, req.ProjectId); err != nil {
		return nil, err
	}
	return api.RevokeEnvironmentFromProject204Response{}, nil
}

// GetProjectEnvironment implements the contract operation: 204 when nothing is selected.
func (a *EnvironmentsAPI) GetProjectEnvironment(ctx context.Context, req api.GetProjectEnvironmentRequestObject) (api.GetProjectEnvironmentResponseObject, error) {
	out, ok, err := a.Service.Selection(ctx, a.OwnerID, req.ProjectId)
	if err != nil {
		return nil, err
	}
	if !ok {
		return api.GetProjectEnvironment204Response{}, nil
	}
	return api.GetProjectEnvironment200JSONResponse(out), nil
}

// SelectProjectEnvironment implements the contract operation.
func (a *EnvironmentsAPI) SelectProjectEnvironment(ctx context.Context, req api.SelectProjectEnvironmentRequestObject) (api.SelectProjectEnvironmentResponseObject, error) {
	out, err := a.Service.Select(ctx, a.OwnerID, req.ProjectId, *req.Body)
	if err != nil {
		return nil, err
	}
	return api.SelectProjectEnvironment200JSONResponse(out), nil
}

// NewAPI builds the environment API. A nil registry client reads public images anonymously from
// the allowlisted registries.
func NewAPI(cfg *config.Config, pool *pgxpool.Pool, client RegistryClient) *EnvironmentsAPI {
	if client == nil {
		client = &AnonymousRegistry{}
	}
	return &EnvironmentsAPI{
		Service: &Service{
			Pool:           pool,
			Validator:      &Validator{Client: client, Allowlist: cfg.EnvironmentRegistryAllowlist, Budgets: DefaultBudgets()},
			FollowInterval: cfg.EnvironmentFollowInterval.Duration(),
		},
		OwnerID: cfg.Identity.OwnerID,
	}
}
