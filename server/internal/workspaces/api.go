package workspaces

import (
	"context"
	"errors"
	"net/http"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
)

// WorkspacesAPI serves the workspace operations of the strict server interface.
type WorkspacesAPI struct {
	Svc *Service
}

var errUnavailable = apierr.Unavailable("Workspaces unavailable")

// failure maps a service error to the answer every workspace operation shares: 404, 409 or 502.
// Errors that are not domain errors are returned as such and become a 500.
func failure(err error) error {
	var rej *RejectedError
	var unc *UnconfirmedError
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrProjectNotFound):
		return apierr.NotFound(notFoundMessage(err))
	case errors.Is(err, ErrBusy):
		return apierr.Conflict("Another change to this workspace is in progress")
	case errors.As(err, &rej):
		return apierr.Conflict(rej.Detail)
	case errors.As(err, &unc):
		return apierr.BadGateway(unc.Detail)
	}
	return err
}

func notFoundMessage(err error) string {
	if errors.Is(err, ErrProjectNotFound) {
		return "Project not found"
	}
	return "Workspace not found"
}

// ListWorkspaces implements api.StrictServerInterface.
func (a *WorkspacesAPI) ListWorkspaces(ctx context.Context, _ api.ListWorkspacesRequestObject) (api.ListWorkspacesResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	ws, err := a.Svc.List(ctx)
	if err != nil {
		return nil, err
	}
	return api.ListWorkspaces200JSONResponse(ws), nil
}

// GetWorkspace implements api.StrictServerInterface.
func (a *WorkspacesAPI) GetWorkspace(ctx context.Context, r api.GetWorkspaceRequestObject) (api.GetWorkspaceResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	w, err := a.Svc.Get(ctx, r.WorkspaceId)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, apierr.NotFound("Workspace not found")
	case err != nil:
		return nil, err
	}
	return api.GetWorkspace200JSONResponse(w), nil
}

// ListWorkspacePreviewPorts implements api.StrictServerInterface.
func (a *WorkspacesAPI) ListWorkspacePreviewPorts(ctx context.Context, r api.ListWorkspacePreviewPortsRequestObject) (api.ListWorkspacePreviewPortsResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	ports, err := a.Svc.Ports(ctx, r.WorkspaceId)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, apierr.NotFound("Workspace not found")
	case err != nil:
		return nil, err
	}
	return api.ListWorkspacePreviewPorts200JSONResponse(api.WorkspacePortsResponse{Ports: ports}), nil
}

// CreateWorkspace implements api.StrictServerInterface.
func (a *WorkspacesAPI) CreateWorkspace(ctx context.Context, r api.CreateWorkspaceRequestObject) (api.CreateWorkspaceResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if r.Body == nil {
		return nil, apierr.Invalid("A JSON body is required")
	}
	w, err := a.Svc.Create(ctx, *r.Body)
	var inv *InvalidError
	var rej *RejectedError
	switch {
	case err == nil:
		return api.CreateWorkspace201JSONResponse(w), nil
	case errors.As(err, &inv):
		return nil, apierr.Invalid(inv.Msg)
	case errors.As(err, &rej):
		// kagent refused the request itself, so the request is what is wrong.
		return nil, apierr.New(http.StatusUnprocessableEntity, rej.Detail)
	}
	return nil, failure(err)
}

// DeleteWorkspace implements api.StrictServerInterface.
func (a *WorkspacesAPI) DeleteWorkspace(ctx context.Context, r api.DeleteWorkspaceRequestObject) (api.DeleteWorkspaceResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	if err := a.Svc.Delete(ctx, r.WorkspaceId); err != nil {
		return nil, failure(err)
	}
	return api.DeleteWorkspace204Response{}, nil
}

// RefreshWorkspace implements api.StrictServerInterface.
func (a *WorkspacesAPI) RefreshWorkspace(ctx context.Context, r api.RefreshWorkspaceRequestObject) (api.RefreshWorkspaceResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	w, err := a.Svc.Refresh(ctx, r.WorkspaceId)
	var rej *RejectedError
	switch {
	case errors.As(err, &rej):
		return nil, apierr.New(http.StatusUnprocessableEntity, rej.Detail)
	case err != nil:
		return nil, failure(err)
	}
	return api.RefreshWorkspace200JSONResponse(w), nil
}

// ResumeWorkspace implements api.StrictServerInterface.
func (a *WorkspacesAPI) ResumeWorkspace(ctx context.Context, r api.ResumeWorkspaceRequestObject) (api.ResumeWorkspaceResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	w, err := a.Svc.Resume(ctx, r.WorkspaceId)
	if err != nil {
		return nil, failure(err)
	}
	return api.ResumeWorkspace200JSONResponse(w), nil
}

// SuspendWorkspace implements api.StrictServerInterface.
func (a *WorkspacesAPI) SuspendWorkspace(ctx context.Context, r api.SuspendWorkspaceRequestObject) (api.SuspendWorkspaceResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	w, err := a.Svc.Suspend(ctx, r.WorkspaceId)
	if err != nil {
		return nil, failure(err)
	}
	return api.SuspendWorkspace200JSONResponse(w), nil
}
