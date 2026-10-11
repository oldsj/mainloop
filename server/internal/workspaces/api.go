package workspaces

import (
	"context"
	"errors"
	"net/http"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/httpx"
)

// WorkspacesAPI serves the workspace operations of the strict server interface.
type WorkspacesAPI struct {
	Svc *Service
}

var errUnavailable = httpx.NewError(http.StatusServiceUnavailable, "Workspaces unavailable")

func notFound() api.HTTPError { return api.HTTPError{Detail: "Workspace not found"} }

func invalid(msg string) api.HTTPValidationError {
	return api.HTTPValidationError{Detail: &[]api.ValidationError{{
		Loc: []api.ValidationError_Loc_Item{}, Msg: msg, Type: "value_error",
	}}}
}

// failure is a service error as an HTTP answer; the per-operation closures place it in the
// response type that operation declares. Errors that are not domain errors are returned as such
// and become a 500.
type failure struct {
	status int
	detail string
}

func asFailure(err error) (failure, bool) {
	var rej *RejectedError
	var unc *UnconfirmedError
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrProjectNotFound):
		return failure{404, notFoundMessage(err)}, true
	case errors.Is(err, ErrBusy):
		return failure{409, "Another change to this workspace is in progress"}, true
	case errors.As(err, &rej):
		return failure{409, rej.Detail}, true
	case errors.As(err, &unc):
		return failure{502, unc.Detail}, true
	}
	return failure{}, false
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
		return api.GetWorkspace404JSONResponse(notFound()), nil
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
		return api.ListWorkspacePreviewPorts404JSONResponse(notFound()), nil
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
		var body api.CreateWorkspace422JSONResponseBody
		if err := body.FromHTTPValidationError(invalid("A JSON body is required")); err != nil {
			return nil, err
		}
		return api.CreateWorkspace422JSONResponse(body), nil
	}
	w, err := a.Svc.Create(ctx, *r.Body)
	var inv *InvalidError
	var rej *RejectedError
	switch {
	case err == nil:
		return api.CreateWorkspace201JSONResponse(w), nil
	case errors.As(err, &inv):
		return create422(func(b *api.CreateWorkspace422JSONResponseBody) error {
			return b.FromHTTPValidationError(invalid(inv.Msg))
		})
	case errors.As(err, &rej):
		// kagent refused the request itself, so the request is what is wrong.
		return create422(func(b *api.CreateWorkspace422JSONResponseBody) error {
			return b.FromHTTPError(api.HTTPError{Detail: rej.Detail})
		})
	}
	f, ok := asFailure(err)
	if !ok {
		return nil, err
	}
	switch f.status {
	case 404:
		return api.CreateWorkspace404JSONResponse{Detail: f.detail}, nil
	case 409:
		return api.CreateWorkspace409JSONResponse{Detail: f.detail}, nil
	}
	return api.CreateWorkspace502JSONResponse{Detail: f.detail}, nil
}

func create422(fill func(*api.CreateWorkspace422JSONResponseBody) error) (api.CreateWorkspaceResponseObject, error) {
	var b api.CreateWorkspace422JSONResponseBody
	if err := fill(&b); err != nil {
		return nil, err
	}
	return api.CreateWorkspace422JSONResponse(b), nil
}

// DeleteWorkspace implements api.StrictServerInterface.
func (a *WorkspacesAPI) DeleteWorkspace(ctx context.Context, r api.DeleteWorkspaceRequestObject) (api.DeleteWorkspaceResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	err := a.Svc.Delete(ctx, r.WorkspaceId)
	if err == nil {
		return api.DeleteWorkspace204Response{}, nil
	}
	f, ok := asFailure(err)
	if !ok {
		return nil, err
	}
	switch f.status {
	case 404:
		return api.DeleteWorkspace404JSONResponse{Detail: f.detail}, nil
	case 409:
		return api.DeleteWorkspace409JSONResponse{Detail: f.detail}, nil
	}
	return api.DeleteWorkspace502JSONResponse{Detail: f.detail}, nil
}

// RefreshWorkspace implements api.StrictServerInterface.
func (a *WorkspacesAPI) RefreshWorkspace(ctx context.Context, r api.RefreshWorkspaceRequestObject) (api.RefreshWorkspaceResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	w, err := a.Svc.Refresh(ctx, r.WorkspaceId)
	var rej *RejectedError
	if errors.As(err, &rej) {
		var b api.RefreshWorkspace422JSONResponseBody
		if err := b.FromHTTPError(api.HTTPError{Detail: rej.Detail}); err != nil {
			return nil, err
		}
		return api.RefreshWorkspace422JSONResponse(b), nil
	}
	if err == nil {
		return api.RefreshWorkspace200JSONResponse(w), nil
	}
	f, ok := asFailure(err)
	if !ok {
		return nil, err
	}
	switch f.status {
	case 404:
		return api.RefreshWorkspace404JSONResponse{Detail: f.detail}, nil
	case 409:
		return api.RefreshWorkspace409JSONResponse{Detail: f.detail}, nil
	}
	return api.RefreshWorkspace502JSONResponse{Detail: f.detail}, nil
}

// ResumeWorkspace implements api.StrictServerInterface.
func (a *WorkspacesAPI) ResumeWorkspace(ctx context.Context, r api.ResumeWorkspaceRequestObject) (api.ResumeWorkspaceResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	w, err := a.Svc.Resume(ctx, r.WorkspaceId)
	if err == nil {
		return api.ResumeWorkspace200JSONResponse(w), nil
	}
	f, ok := asFailure(err)
	if !ok {
		return nil, err
	}
	switch f.status {
	case 404:
		return api.ResumeWorkspace404JSONResponse{Detail: f.detail}, nil
	case 409:
		return api.ResumeWorkspace409JSONResponse{Detail: f.detail}, nil
	}
	return api.ResumeWorkspace502JSONResponse{Detail: f.detail}, nil
}

// SuspendWorkspace implements api.StrictServerInterface.
func (a *WorkspacesAPI) SuspendWorkspace(ctx context.Context, r api.SuspendWorkspaceRequestObject) (api.SuspendWorkspaceResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	w, err := a.Svc.Suspend(ctx, r.WorkspaceId)
	if err == nil {
		return api.SuspendWorkspace200JSONResponse(w), nil
	}
	f, ok := asFailure(err)
	if !ok {
		return nil, err
	}
	switch f.status {
	case 404:
		return api.SuspendWorkspace404JSONResponse{Detail: f.detail}, nil
	case 409:
		return api.SuspendWorkspace409JSONResponse{Detail: f.detail}, nil
	}
	return api.SuspendWorkspace502JSONResponse{Detail: f.detail}, nil
}
