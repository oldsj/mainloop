package projects

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// Bounds of a workspace service command: kagent's Workspace.service_command, less the three
// arguments a workspace adds to set its preview environment.
const (
	maxServiceArgs   = 61
	maxServiceArgLen = 4096
)

// WorkspaceService returns the argv of the project's workspace service; empty declares none.
func (s *Service) WorkspaceService(ctx context.Context, id string) ([]string, error) {
	p, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return p.WorkspaceServiceCommand, nil
}

// SetWorkspaceService replaces the project's workspace service command. Workspaces created
// afterwards copy it; existing ones keep theirs.
func (s *Service) SetWorkspaceService(ctx context.Context, id string, command []string) ([]string, error) {
	if !validServiceCommand(command) {
		return nil, ErrInvalidService
	}
	if command == nil {
		command = []string{}
	}
	p, err := dbq.New(s.Pool).SetOwnedProjectWorkspaceService(ctx, dbq.SetOwnedProjectWorkspaceServiceParams{
		ID: id, UserID: s.OwnerID, WorkspaceServiceCommand: command,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("set workspace service: %w", err)
	}
	return p.WorkspaceServiceCommand, nil
}

func validServiceCommand(command []string) bool {
	return len(command) <= maxServiceArgs && !slices.ContainsFunc(command, func(arg string) bool {
		return arg == "" || len(arg) > maxServiceArgLen
	})
}

// GetProjectWorkspaceService implements api.StrictServerInterface.
func (a *ProjectsAPI) GetProjectWorkspaceService(ctx context.Context, r api.GetProjectWorkspaceServiceRequestObject) (api.GetProjectWorkspaceServiceResponseObject, error) {
	command, err := a.Svc.WorkspaceService(ctx, r.ProjectId)
	if errors.Is(err, ErrNotFound) {
		return nil, apierr.NotFound("Project not found")
	}
	if err != nil {
		return nil, err
	}
	return api.GetProjectWorkspaceService200JSONResponse{Command: command}, nil
}

// UpdateProjectWorkspaceService implements api.StrictServerInterface: owner-only, Mainloop-side
// configuration, never read from the project's own repository.
func (a *ProjectsAPI) UpdateProjectWorkspaceService(ctx context.Context, r api.UpdateProjectWorkspaceServiceRequestObject) (api.UpdateProjectWorkspaceServiceResponseObject, error) {
	command, err := a.Svc.SetWorkspaceService(ctx, r.ProjectId, r.Body.Command)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, apierr.NotFound("Project not found")
	case errors.Is(err, ErrInvalidService):
		return nil, apierr.Invalid("A workspace service command has at most 61 non-empty arguments of at most 4096 bytes")
	case err != nil:
		return nil, err
	}
	return api.UpdateProjectWorkspaceService200JSONResponse{Command: command}, nil
}
