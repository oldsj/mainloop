package projects

import (
	"context"
	"errors"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/devloop"
)

func toSyncPath(p devloop.SyncPath) api.DevloopSyncPath {
	v := api.DevloopSyncPath{Selector: p.Selector, Container: p.Container, Prefix: p.Prefix, Dest: p.Dest}
	if len(p.Exclude) > 0 {
		v.Exclude = &p.Exclude
	}
	return v
}

func fromSyncPath(p api.DevloopSyncPath) devloop.SyncPath {
	sp := devloop.SyncPath{Selector: p.Selector, Container: p.Container, Prefix: p.Prefix, Dest: p.Dest}
	if p.Exclude != nil {
		sp.Exclude = *p.Exclude
	}
	return sp
}

func toDevloopTarget(t devloop.Target) api.DevloopTarget {
	v := api.DevloopTarget{
		Id: t.ID, ProjectId: t.ProjectID, Kind: api.DevloopTargetKind(t.Kind), Namespace: t.Namespace,
		Workload: t.Workload, CommandContainer: t.CommandContainer,
		SyncPaths:  make([]api.DevloopSyncPath, 0, len(t.SyncPaths)),
		RestartCmd: t.RestartCmd, MigrateCmd: t.MigrateCmd, HealthCmd: t.HealthCmd, SmokeCmd: t.SmokeCmd,
	}
	for _, p := range t.SyncPaths {
		v.SyncPaths = append(v.SyncPaths, toSyncPath(p))
	}
	if t.VClusterName != "" {
		v.VclusterName = &t.VClusterName
	}
	if t.VClusterNamespace != "" {
		v.VclusterNamespace = &t.VClusterNamespace
	}
	if t.VClusterServer != "" {
		v.VclusterServer = &t.VClusterServer
	}
	return v
}

func fromDevloopTargetUpdate(u api.DevloopTargetUpdate) devloop.Target {
	t := devloop.Target{
		Kind: devloop.TargetKind(u.Kind), Namespace: u.Namespace, Workload: u.Workload, CommandContainer: u.CommandContainer,
		RestartCmd: u.RestartCmd, MigrateCmd: u.MigrateCmd, HealthCmd: u.HealthCmd, SmokeCmd: u.SmokeCmd,
	}
	for _, p := range u.SyncPaths {
		t.SyncPaths = append(t.SyncPaths, fromSyncPath(p))
	}
	if u.VclusterName != nil {
		t.VClusterName = *u.VclusterName
	}
	if u.VclusterNamespace != nil {
		t.VClusterNamespace = *u.VclusterNamespace
	}
	if u.VclusterServer != nil {
		t.VClusterServer = *u.VclusterServer
	}
	return t
}

// GetProjectDevloopTarget implements api.StrictServerInterface: the owner's read of a project's
// staging target. A task's own project is also visible to it, through the devloop MCP tools
// reading the same [devloop.TargetService], not through this route.
func (a *ProjectsAPI) GetProjectDevloopTarget(ctx context.Context, r api.GetProjectDevloopTargetRequestObject) (api.GetProjectDevloopTargetResponseObject, error) {
	if a.Devloop == nil {
		return nil, apierr.NotFound("Project not found")
	}
	if _, err := a.Svc.Get(ctx, r.ProjectId); errors.Is(err, ErrNotFound) {
		return nil, apierr.NotFound("Project not found")
	} else if err != nil {
		return nil, err
	}
	target, err := a.Devloop.Get(ctx, r.ProjectId)
	switch {
	case errors.Is(err, devloop.ErrTargetNotFound):
		return nil, apierr.NotFound("This project has no staging target configured")
	case err != nil:
		return nil, err
	}
	return api.GetProjectDevloopTarget200JSONResponse(toDevloopTarget(target)), nil
}

// UpdateProjectDevloopTarget implements api.StrictServerInterface: owner-only. Mainloop-side
// configuration only, never a file in the project's own repository (the owner works on arbitrary
// and third-party repos).
func (a *ProjectsAPI) UpdateProjectDevloopTarget(ctx context.Context, r api.UpdateProjectDevloopTargetRequestObject) (api.UpdateProjectDevloopTargetResponseObject, error) {
	if a.Devloop == nil {
		return nil, apierr.NotFound("Project not found")
	}
	if _, err := a.Svc.Get(ctx, r.ProjectId); errors.Is(err, ErrNotFound) {
		return nil, apierr.NotFound("Project not found")
	} else if err != nil {
		return nil, err
	}
	target, err := a.Devloop.Set(ctx, r.ProjectId, fromDevloopTargetUpdate(*r.Body))
	if errors.Is(err, devloop.ErrInvalidTarget) {
		return nil, apierr.New(400, err.Error())
	}
	if err != nil {
		return nil, err
	}
	return api.UpdateProjectDevloopTarget200JSONResponse(toDevloopTarget(target)), nil
}
