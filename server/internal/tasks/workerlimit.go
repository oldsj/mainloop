package tasks

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
)

// MaxWorkerLimit is the highest limit a project may set on its worker tasks.
const MaxWorkerLimit = 100

// WorkerLimit is a project's limit on its worker tasks (child tasks) holding a slot at once
// (docs/specs/tasks.md#capacity).
type WorkerLimit struct {
	ProjectID string
	// Max is the project's own limit; nil uses Default.
	Max *int
	// Default is the limit of a project that sets none. Effective is the most workers the project
	// can have: its own limit or the default, capped by Global.
	Default, Effective int
	// Global is the owner-wide limit on child tasks, which applies as well.
	Global int
	// Running is how many of the project's worker tasks hold a slot now.
	Running int
}

// LimitChanger is who changes a project's worker limit: the owner through the API, or the main
// session (SessionID) through the project_settings tool.
type LimitChanger struct {
	SessionID string
}

func (c LimitChanger) by() string {
	if c.SessionID != "" {
		return dbstate.WorkerLimitByMain
	}
	return dbstate.WorkerLimitByOwner
}

// projectWorkerLimit is the limit admission applies to the project's workers: its own, or the
// default. 0 is none.
func (s *Service) projectWorkerLimit(ctx context.Context, q *dbq.Queries, owner, project string) (int, error) {
	m, err := q.GetProjectMaxWorkers(ctx, dbq.GetProjectMaxWorkersParams{ID: project, UserID: owner})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return s.Limits.MaxWorkersPerProject, nil // the create refuses a project not the owner's
	case err != nil:
		return 0, err
	case m != nil:
		return int(*m), nil
	}
	return s.Limits.MaxWorkersPerProject, nil
}

// WorkerLimit reads the owner's project's worker limit and how many of its workers hold a slot.
func (s *Service) WorkerLimit(ctx context.Context, owner, project string) (WorkerLimit, error) {
	q := dbq.New(s.Pool)
	m, err := q.GetProjectMaxWorkers(ctx, dbq.GetProjectMaxWorkersParams{ID: project, UserID: owner})
	if err != nil {
		return WorkerLimit{}, notFoundOr(err, ReasonProjectNotFound)
	}
	n, err := q.CountProjectCapacity(ctx, dbq.CountProjectCapacityParams{OwnerID: owner, ProjectID: &project})
	if err != nil {
		return WorkerLimit{}, err
	}
	w := WorkerLimit{
		ProjectID: project, Default: s.Limits.MaxWorkersPerProject, Effective: s.Limits.MaxWorkersPerProject,
		Global: s.Limits.MaxChildrenGlobal, Running: int(n),
	}
	if m != nil {
		v := int(*m)
		w.Max, w.Effective = &v, v
	}
	if w.Global > 0 {
		w.Effective = min(w.Effective, w.Global)
	}
	return w, nil
}

// SetWorkerLimit sets the owner's project's own worker limit, or with nil returns it to the
// default, and records the change and who made it. Running workers are not stopped: a limit below
// their number only refuses new ones until enough end. Setting the limit it already has records
// nothing. The change takes the owner's capacity lock, so an admission sees the old limit or the
// new one.
func (s *Service) SetWorkerLimit(ctx context.Context, owner, project string, max *int, by LimitChanger) (WorkerLimit, error) {
	if max != nil && (*max < 1 || *max > MaxWorkerLimit) {
		return WorkerLimit{}, refusal(422, ReasonInvalidRequest)
	}
	err := s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		if err := q.LockTaskRequest(ctx, "capacity\x1f"+owner); err != nil {
			return nil, err
		}
		old, err := q.LockProjectMaxWorkers(ctx, dbq.LockProjectMaxWorkersParams{ID: project, UserID: owner})
		if err != nil {
			return nil, notFoundOr(err, ReasonProjectNotFound)
		}
		next := int32Ptr(max)
		if (old == nil) == (next == nil) && (old == nil || *old == *next) {
			return nil, nil
		}
		if err := q.SetProjectMaxWorkers(ctx, dbq.SetProjectMaxWorkersParams{ID: project, MaxWorkers: next}); err != nil {
			return nil, err
		}
		return nil, q.InsertWorkerLimitAudit(ctx, dbq.InsertWorkerLimitAuditParams{
			ProjectID: project, OwnerID: owner, ChangedBy: by.by(), SessionID: ptr(by.SessionID),
			OldMaxWorkers: old, NewMaxWorkers: next,
		})
	})
	if err != nil {
		return WorkerLimit{}, err
	}
	return s.WorkerLimit(ctx, owner, project)
}

func int32Ptr(v *int) *int32 {
	if v == nil {
		return nil
	}
	n := int32(*v) //nolint:gosec // bounded by MaxWorkerLimit
	return &n
}
