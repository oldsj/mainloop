package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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
	// maxServicePorts keeps MAINLOOP_PREVIEW_URLS, one `<port>=<url>` per port, inside one
	// 4096-byte argument.
	maxServicePorts = 16
)

var portNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidatePorts checks preview ports are well formed and unique by name and number. Workspaces
// check their own dev.ports with it too.
func ValidatePorts(ports []api.WorkspacePort) error {
	names, numbers := map[string]bool{}, map[int]bool{}
	for _, p := range ports {
		if !portNameRE.MatchString(p.Name) || p.Number < 1 || p.Number > 65535 {
			return fmt.Errorf("workspace port %q is not valid", p.Name)
		}
		if names[p.Name] || numbers[p.Number] {
			return fmt.Errorf("workspace port names and numbers must be unique")
		}
		names[p.Name], numbers[p.Number] = true, true
	}
	return nil
}

// ServicePorts decodes a project's stored workspace service ports. A row the migration's check
// admitted always decodes; an error means the column was written outside this package.
func ServicePorts(p dbq.Project) ([]api.WorkspacePort, error) {
	ports := []api.WorkspacePort{}
	if len(p.WorkspaceServicePorts) == 0 {
		return ports, nil
	}
	if err := json.Unmarshal(p.WorkspaceServicePorts, &ports); err != nil {
		return nil, fmt.Errorf("project %s workspace service ports: %w", p.ID, err)
	}
	return ports, nil
}

// WorkspaceService returns the project's workspace service: its argv (empty declares none) and the
// preview ports of its workspaces.
func (s *Service) WorkspaceService(ctx context.Context, id string) (api.WorkspaceService, error) {
	p, err := s.Get(ctx, id)
	if err != nil {
		return api.WorkspaceService{}, err
	}
	return workspaceService(p)
}

// SetWorkspaceService replaces the project's workspace service command and ports. Workspaces
// created afterwards copy them; existing ones keep theirs.
func (s *Service) SetWorkspaceService(ctx context.Context, id string, in api.WorkspaceService) (api.WorkspaceService, error) {
	if err := ValidateWorkspaceService(in); err != nil {
		return api.WorkspaceService{}, err
	}
	command, ports := in.Command, in.Ports
	if command == nil {
		command = []string{}
	}
	if ports == nil {
		ports = []api.WorkspacePort{}
	}
	portJSON, err := json.Marshal(ports)
	if err != nil {
		return api.WorkspaceService{}, err
	}
	p, err := dbq.New(s.Pool).SetOwnedProjectWorkspaceService(ctx, dbq.SetOwnedProjectWorkspaceServiceParams{
		ID: id, UserID: s.OwnerID, WorkspaceServiceCommand: command, WorkspaceServicePorts: portJSON,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return api.WorkspaceService{}, ErrNotFound
	}
	if err != nil {
		return api.WorkspaceService{}, fmt.Errorf("set workspace service: %w", err)
	}
	return workspaceService(p)
}

func workspaceService(p dbq.Project) (api.WorkspaceService, error) {
	ports, err := ServicePorts(p)
	if err != nil {
		return api.WorkspaceService{}, err
	}
	return api.WorkspaceService{Command: p.WorkspaceServiceCommand, Ports: ports}, nil
}

// ValidateWorkspaceService checks a workspace service against the contract's bounds, as a save
// does: ErrInvalidService for the command, ErrInvalidServicePorts for the ports.
func ValidateWorkspaceService(in api.WorkspaceService) error {
	if !validServiceCommand(in.Command) {
		return ErrInvalidService
	}
	if len(in.Ports) > maxServicePorts || ValidatePorts(in.Ports) != nil {
		return ErrInvalidServicePorts
	}
	return nil
}

func validServiceCommand(command []string) bool {
	return len(command) <= maxServiceArgs && !slices.ContainsFunc(command, func(arg string) bool {
		return arg == "" || len(arg) > maxServiceArgLen
	})
}

// GetProjectWorkspaceService implements api.StrictServerInterface.
func (a *ProjectsAPI) GetProjectWorkspaceService(ctx context.Context, r api.GetProjectWorkspaceServiceRequestObject) (api.GetProjectWorkspaceServiceResponseObject, error) {
	service, err := a.Svc.WorkspaceService(ctx, r.ProjectId)
	if errors.Is(err, ErrNotFound) {
		return nil, apierr.NotFound("Project not found")
	}
	if err != nil {
		return nil, err
	}
	return api.GetProjectWorkspaceService200JSONResponse(service), nil
}

// UpdateProjectWorkspaceService implements api.StrictServerInterface: owner-only, Mainloop-side
// configuration, never read from the project's own repository.
func (a *ProjectsAPI) UpdateProjectWorkspaceService(ctx context.Context, r api.UpdateProjectWorkspaceServiceRequestObject) (api.UpdateProjectWorkspaceServiceResponseObject, error) {
	service, err := a.Svc.SetWorkspaceService(ctx, r.ProjectId, *r.Body)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, apierr.NotFound("Project not found")
	case errors.Is(err, ErrInvalidService):
		return nil, apierr.Invalid("A workspace service command has at most 61 non-empty arguments of at most 4096 bytes")
	case errors.Is(err, ErrInvalidServicePorts):
		return nil, apierr.Invalid("Workspace service ports are at most 16, each named [a-z][a-z0-9-]{0,31} with a number from 1 to 65535, unique by name and number")
	case err != nil:
		return nil, err
	}
	return api.UpdateProjectWorkspaceService200JSONResponse(service), nil
}
