package tasks

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
)

// standingRequestID is the create request of a project's standing supervisor. It is the owner's,
// whoever's first use made it, so the supervisor belongs to the project and not to a caller.
func standingRequestID(projectID string) string { return "standing-supervisor:" + projectID }

// StandingBrief is the stored brief of a project's standing supervisor. What it is and how it
// works is rendered around it on every attempt (see [DelegatedBrief]), so a change of wording
// reaches the next attempt without rewriting the task.
func StandingBrief(fullName string) string {
	return "Be the standing supervisor of " + fullName + ". Work arrives as messages from main and the owner."
}

// EnsureSupervisor answers the project's standing supervisor, creating it on first use
// (docs/specs/project-supervisor.md). A project has one for its life: concurrent first uses
// serialize on a per-project lock and the unique index behind it, and make one task.
func (s *Service) EnsureSupervisor(ctx context.Context, owner, projectID string) (api.TaskView, error) {
	var id string
	err := events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		if err := q.LockTaskRequest(ctx, "standing\x1f"+projectID); err != nil {
			return nil, err
		}
		project, err := q.GetProject(ctx, projectID)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && project.UserID != owner {
			return nil, refusal(404, ReasonProjectNotFound)
		}
		if err != nil {
			return nil, err
		}
		existing, err := q.GetStandingTask(ctx, &projectID)
		if err == nil {
			id = existing.ID
			return nil, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		in := CreateInput{
			RequestID: standingRequestID(projectID), Title: "Supervisor of " + project.FullName,
			Brief: StandingBrief(project.FullName), Mode: dbstate.TaskModeCoordination,
			ProjectID: projectID, standing: true,
		}
		dig, payload, err := digest(dbstate.TaskOpCreate, "", in.payload())
		if err != nil {
			return nil, err
		}
		_, t, err := s.createTx(ctx, q, OwnerPrincipal(owner), in, dig, payload)
		if err != nil {
			return nil, err
		}
		id = t.ID
		return []events.Draft{s.updated(t)}, nil
	})
	if err != nil {
		s.noteRefusal(ctx, err)
		return api.TaskView{}, err
	}
	return s.Get(ctx, OwnerPrincipal(owner), id)
}
