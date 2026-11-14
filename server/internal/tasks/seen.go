package tasks

// The owner's seen state (docs/specs/seen.md): when each task was last opened, and the last two
// app visits, kept per owner rather than per browser so it follows them across devices.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// SeenKeep is how long a task's last-opened time is kept. An older one is dropped and the task
// reads as seen, as it did in local storage.
const SeenKeep = 60 * 24 * time.Hour

// Seen reads the owner's seen state. With no record yet nothing is unseen: since is now.
func (s *Service) Seen(ctx context.Context, owner string) (api.Seen, error) {
	return s.seen(ctx, dbq.New(s.Pool), owner)
}

func (s *Service) seen(ctx context.Context, q *dbq.Queries, owner string) (api.Seen, error) {
	now := s.now()
	out := api.Seen{Since: now, Tasks: []api.SeenTask{}}
	row, err := q.GetOwnerSeen(ctx, owner)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return out, nil
	case err != nil:
		return api.Seen{}, fmt.Errorf("read seen: %w", err)
	}
	out.Since, out.LastVisit, out.PreviousVisit = row.Since, row.LastVisit, row.PreviousVisit
	tasks, err := q.ListTaskSeen(ctx, dbq.ListTaskSeenParams{UserID: owner, SeenAt: now.Add(-SeenKeep)})
	if err != nil {
		return api.Seen{}, fmt.Errorf("read seen tasks: %w", err)
	}
	for _, t := range tasks {
		out.Tasks = append(out.Tasks, api.SeenTask{TaskId: t.TaskID, SeenAt: t.SeenAt})
	}
	return out, nil
}

// RecordVisit records that the owner opened the app now and returns the state after it, whose
// previous_visit is the visit before this one on any device.
func (s *Service) RecordVisit(ctx context.Context, owner string) (api.Seen, error) {
	var out api.Seen
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := q.EnsureOwnerSeen(ctx, owner); err != nil {
			return err
		}
		if _, err := q.RecordVisit(ctx, owner); err != nil {
			return err
		}
		var err error
		out, err = s.seen(ctx, q, owner)
		return err
	})
	return out, err
}

// MarkTaskSeen records that the owner has one of their tasks open now, by the database's clock,
// which also stamps the task's own changes. It reports false when the owner has no such task.
func (s *Service) MarkTaskSeen(ctx context.Context, owner, taskID string) (api.SeenTask, bool, error) {
	var out api.SeenTask
	found := true
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := q.EnsureOwnerSeen(ctx, owner); err != nil {
			return err
		}
		row, err := q.MarkTaskSeen(ctx, dbq.MarkTaskSeenParams{UserID: owner, TaskID: taskID})
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
			return nil
		}
		if err != nil {
			return err
		}
		out = api.SeenTask{TaskId: row.TaskID, SeenAt: row.SeenAt}
		return q.PruneTaskSeen(ctx, dbq.PruneTaskSeenParams{UserID: owner, SeenAt: s.now().Add(-SeenKeep)})
	})
	return out, found, err
}

// GetSeen implements api.StrictServerInterface.
func (a *TasksAPI) GetSeen(ctx context.Context, _ api.GetSeenRequestObject) (api.GetSeenResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	out, err := a.Svc.Seen(ctx, a.OwnerID)
	if err != nil {
		return nil, err
	}
	return api.GetSeen200JSONResponse(out), nil
}

// RecordVisit implements api.StrictServerInterface.
func (a *TasksAPI) RecordVisit(ctx context.Context, _ api.RecordVisitRequestObject) (api.RecordVisitResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	out, err := a.Svc.RecordVisit(ctx, a.OwnerID)
	if err != nil {
		return nil, err
	}
	return api.RecordVisit200JSONResponse(out), nil
}

// MarkTaskSeen implements api.StrictServerInterface.
func (a *TasksAPI) MarkTaskSeen(ctx context.Context, req api.MarkTaskSeenRequestObject) (api.MarkTaskSeenResponseObject, error) {
	if a.Svc == nil {
		return nil, errUnavailable
	}
	out, found, err := a.Svc.MarkTaskSeen(ctx, a.OwnerID, req.TaskId)
	switch {
	case err != nil:
		return nil, err
	case !found:
		return nil, apierr.NotFound("Task not found")
	}
	return api.MarkTaskSeen200JSONResponse(out), nil
}
