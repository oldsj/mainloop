package devloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// TargetKind is how a target's Kubernetes location is reached. It is the one thing that varies
// between "a namespace on the cluster serve-api already reaches" and "a namespace inside a
// vcluster serve-api reaches through that vcluster's own API server"; everything else about a
// sync (paths, commands) is the same shape either way, which is what keeps the mechanism
// language- and project-agnostic rather than tied to Mainloop's own Go/React layout.
type TargetKind = dbstate.DevloopTargetKind

// Target kinds.
const (
	TargetNamespace = dbstate.DevloopTargetNamespace
	TargetVCluster  = dbstate.DevloopTargetVCluster
)

// LeaseID is oldsj/mainloop's own devloop target id, seeded by migration
// (00310_devloop_targets.sql) onto the devloop_lease and devloop_sync_lock rows the original
// devloop migration already created. The owner API's /devloop/lease routes and `make dev-live`
// act on this one target by name, by owner decision (docs/specs/devloop.md): mainloop-dev itself
// is not renamed or made configurable by this package. A task agent's lease, sync and status
// calls use its own project's target id instead (see [TargetService.Get]), which equals this
// constant for oldsj/mainloop and is independent for every other project.
const LeaseID = "mainloop-dev"

// SyncPath is one container a sync copies into: which repository subtree (Prefix, relative to the
// project root) lands at Dest inside Container, in the pod Selector matches, and which paths under
// Prefix a copy never writes or deletes (the same shape FilterTar and CopyTree already take).
type SyncPath struct {
	Selector  string   `json:"selector"`
	Container string   `json:"container"`
	Prefix    string   `json:"prefix"`
	Dest      string   `json:"dest"`
	Exclude   []string `json:"exclude,omitempty"`
}

// Target is a project's staging target: where its code runs, and the commands a sync uses to
// bring it up to date. Mainloop-side configuration only, set through the project API, never a
// file in the project's own repository (the owner works on arbitrary and third-party repos).
type Target struct {
	ID        string
	ProjectID string
	Kind      TargetKind
	// Namespace is the Kubernetes namespace the workload runs in for TargetNamespace, or the host
	// namespace holding the vcluster for TargetVCluster.
	Namespace string
	// VClusterName and VClusterNamespace locate the workload inside the vcluster's own API
	// server; both set for TargetVCluster, both empty for TargetNamespace.
	VClusterName      string
	VClusterNamespace string
	// Workload is a label selector (e.g. "app=mainloop-backend") identifying the pod that
	// RestartCmd, MigrateCmd, HealthCmd and SmokeCmd run in, inside CommandContainer.
	Workload         string
	CommandContainer string
	SyncPaths        []SyncPath
	// Each command is optional: a target without one skips that step rather than failing it.
	RestartCmd, MigrateCmd, HealthCmd, SmokeCmd *string
}

// ErrTargetNotFound means the project has no staging target configured.
var ErrTargetNotFound = errors.New("devloop: no staging target is configured for this project")

// ErrInvalidTarget means a target's fields do not describe a workable target (see
// [TargetService.Set]).
var ErrInvalidTarget = errors.New("devloop: invalid staging target")

// TargetService reads and writes devloop_targets. Reads are called from both serve-mcp (to decide
// whether the devloop tools are available to a session's project, and which lease and sync lock
// to use) and serve-api (to drive a sync); writes are called only from the owner-only project API
// (projects.ProjectsAPI), never from a task's workspace.
type TargetService struct {
	Pool *pgxpool.Pool
}

// Get reads projectID's staging target, or [ErrTargetNotFound] if none is configured.
func (s *TargetService) Get(ctx context.Context, projectID string) (Target, error) {
	row, err := dbq.New(s.Pool).GetDevloopTargetByProject(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Target{}, ErrTargetNotFound
	}
	if err != nil {
		return Target{}, err
	}
	return fromRow(row)
}

// Set creates or replaces projectID's staging target. An existing target keeps its id (and so its
// lease and sync lock), so a later edit never orphans a sync in flight; a new target gets a fresh
// id, with its lease and sync lock rows created alongside it.
func (s *TargetService) Set(ctx context.Context, projectID string, t Target) (Target, error) {
	if err := validateTarget(t); err != nil {
		return Target{}, err
	}
	syncPaths, err := json.Marshal(t.SyncPaths)
	if err != nil {
		return Target{}, err
	}
	var out Target
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		id := uuid.NewString()
		if existing, err := q.GetDevloopTargetByProject(ctx, projectID); err == nil {
			id = existing.ID
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		row, err := q.UpsertDevloopTarget(ctx, dbq.UpsertDevloopTargetParams{
			ID: id, ProjectID: projectID, Kind: t.Kind, Namespace: t.Namespace,
			VclusterName: nilIfEmpty(t.VClusterName), VclusterNamespace: nilIfEmpty(t.VClusterNamespace),
			Workload: t.Workload, CommandContainer: t.CommandContainer, SyncPaths: syncPaths,
			RestartCmd: t.RestartCmd, MigrateCmd: t.MigrateCmd, HealthCmd: t.HealthCmd, SmokeCmd: t.SmokeCmd,
		})
		if err != nil {
			return err
		}
		if err := q.EnsureDevloopLease(ctx, id); err != nil {
			return err
		}
		if err := q.EnsureDevloopSyncLock(ctx, id); err != nil {
			return err
		}
		out, err = fromRow(row)
		return err
	})
	if err != nil {
		return Target{}, err
	}
	return out, nil
}

// validateTarget rejects a target whose fields cannot describe a workable sync: the command pod
// (Workload, CommandContainer) must be one its own sync paths actually copy into, since that is
// where the commit marker that guards every command step is written and checked.
func validateTarget(t Target) error {
	if t.Namespace == "" || t.Workload == "" || t.CommandContainer == "" {
		return fmt.Errorf("%w: namespace, workload and command_container are required", ErrInvalidTarget)
	}
	switch t.Kind {
	case TargetNamespace:
	case TargetVCluster:
		if t.VClusterName == "" || t.VClusterNamespace == "" {
			return fmt.Errorf("%w: a vcluster target needs vcluster_name and vcluster_namespace", ErrInvalidTarget)
		}
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidTarget, t.Kind)
	}
	for _, p := range t.SyncPaths {
		if p.Selector == "" || p.Container == "" || p.Prefix == "" || p.Dest == "" {
			return fmt.Errorf("%w: every sync path needs selector, container, prefix and dest", ErrInvalidTarget)
		}
	}
	if _, ok := commandSyncPath(t); !ok {
		return fmt.Errorf("%w: workload/command_container must match one sync path's selector/container", ErrInvalidTarget)
	}
	return nil
}

// commandSyncPath is the sync path restart_cmd, migrate_cmd, health_cmd and smoke_cmd run
// alongside: the one whose selector and container match the target's Workload and
// CommandContainer. Its Dest is where the commit marker lives.
func commandSyncPath(t Target) (SyncPath, bool) {
	for _, p := range t.SyncPaths {
		if p.Selector == t.Workload && p.Container == t.CommandContainer {
			return p, true
		}
	}
	return SyncPath{}, false
}

func fromRow(row dbq.DevloopTarget) (Target, error) {
	var paths []SyncPath
	if len(row.SyncPaths) > 0 {
		if err := json.Unmarshal(row.SyncPaths, &paths); err != nil {
			return Target{}, err
		}
	}
	t := Target{
		ID: row.ID, ProjectID: row.ProjectID, Kind: row.Kind, Namespace: row.Namespace,
		Workload: row.Workload, CommandContainer: row.CommandContainer, SyncPaths: paths,
		RestartCmd: row.RestartCmd, MigrateCmd: row.MigrateCmd, HealthCmd: row.HealthCmd, SmokeCmd: row.SmokeCmd,
	}
	if row.VclusterName != nil {
		t.VClusterName = *row.VclusterName
	}
	if row.VclusterNamespace != nil {
		t.VClusterNamespace = *row.VclusterNamespace
	}
	return t, nil
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
