package tasks

// Holds: the main thread sets a task aside until a time (docs/specs/tasks.md#holds). A hold is
// not a status: it only keeps the task out of the owner's Working and Needs you views. Triggers in
// the database clear it the moment the task stops being live or something needs the owner about
// it, so a hold never hides a failure; the hold sweep clears one that runs out and tells main.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// Hold bounds.
const (
	MaxHold          = 7 * 24 * time.Hour
	MaxHoldReason    = 500
	defaultHoldEvery = time.Minute
	holdBatch        = 100
)

// HoldInput sets or clears a hold. A zero Until clears it.
type HoldInput struct {
	TaskID string
	Until  time.Time
	Reason string
}

// Hold sets or clears a hold on one of the owner's tasks. Only the main thread (or the owner)
// holds: a task agent cannot set its own work aside. A hold is refused on a task that is not live
// or has something open for the owner, since it would be cleared at once.
func (s *Service) Hold(ctx context.Context, p Principal, in HoldInput) (api.Task, error) {
	if p.Role != "" && p.Role != dbstate.RoleMain {
		return api.Task{}, refusal(403, ReasonManagementDenied)
	}
	set := !in.Until.IsZero()
	if set {
		if d := in.Until.Sub(s.now()); d <= 0 || d > MaxHold {
			return api.Task{}, refusal(422, ReasonInvalidRequest)
		}
		if len(in.Reason) > MaxHoldReason {
			return api.Task{}, refusal(422, ReasonInvalidRequest)
		}
	}
	var out dbq.Task
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		t, err := q.GetTaskForUpdate(ctx, dbq.GetTaskForUpdateParams{ID: in.TaskID, OwnerID: p.OwnerID})
		if err != nil {
			return notFoundOr(err, ReasonTaskNotFound)
		}
		params := dbq.SetTaskHoldParams{ID: t.ID}
		if set {
			if !holdable(t.Status) {
				return refusal(409, ReasonHoldNotLive)
			}
			// The lock the attention trigger takes (migration 00470): an item committed before
			// this is counted below, and one inserted after waits and then clears the hold.
			if err := db.XactLock(ctx, tx, "mainloop:task-hold:"+t.ID); err != nil {
				return err
			}
			n, err := q.CountOpenTaskAttention(ctx, &t.ID)
			if err != nil {
				return err
			}
			if n > 0 {
				return refusal(409, ReasonHoldNeedsOwner)
			}
			until := in.Until.UTC()
			params.HeldUntil = &until
			if in.Reason != "" {
				params.HeldReason = &in.Reason
			}
		}
		out, err = q.SetTaskHold(ctx, params)
		return err
	})
	if err != nil {
		return api.Task{}, err
	}
	return toTask(out)
}

func holdable(st dbstate.TaskStatus) bool {
	switch st {
	case dbstate.TaskQueued, dbstate.TaskRunning, dbstate.TaskWaiting:
		return true
	}
	return false
}

// HoldSweep ends the holds that have run out: it tells the main thread, once per hold, then
// clears the hold, so the task is back in the owner's views.
func (s *Service) HoldSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "task-holds", Every: defaultHoldEvery, Run: s.runHolds}
}

func (s *Service) runHolds(ctx context.Context) error {
	now := s.now()
	rows, err := dbq.New(s.Pool).ListExpiredHolds(ctx, dbq.ListExpiredHoldsParams{HeldUntil: &now, Limit: holdBatch})
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range rows {
		if err := s.endHold(ctx, t); err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
}

// holdMessageID is the ledger id of a hold's nudge. It is derived from the task and the hold's
// time, so a sweep that sent it but lost the clear finds the message instead of sending another.
func holdMessageID(taskID string, until time.Time) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, fmt.Appendf(nil, "mainloop:task-hold:%s:%d", taskID, until.UnixNano())).String()
}

// endHold sends the nudge (no transaction is open across it), then clears the hold only if it is
// still the same one: a hold main moved meanwhile is left for its own time.
func (s *Service) endHold(ctx context.Context, t dbq.Task) error {
	until := *t.HeldUntil
	mid := holdMessageID(t.ID, until)
	q := dbq.New(s.Pool)
	if _, err := q.GetDelivery(ctx, mid); errors.Is(err, pgx.ErrNoRows) {
		_, err := s.Sessions.SendAs(ctx, sessions.SourceMainloop, mid, "", holdText(t))
		switch {
		case errors.Is(err, sessions.ErrBusy), errors.Is(err, sessions.ErrConflict), errors.Is(err, sessions.ErrNotFound):
			return nil // the next sweep tries again
		case err != nil:
			return err
		}
	} else if err != nil {
		return err
	}
	_, err := q.ReleaseExpiredHold(ctx, dbq.ReleaseExpiredHoldParams{ID: t.ID, HeldUntil: &until})
	return err
}

func holdText(t dbq.Task) string {
	line := fmt.Sprintf("[mainloop] The hold on task %s (%q) ended at %s; it is %s and back in the owner's views.",
		t.ID, t.Title, t.HeldUntil.UTC().Format(time.RFC3339), t.Status)
	if t.HeldReason != nil {
		line += fmt.Sprintf(" You held it because: %s", *t.HeldReason)
	}
	return line + " Look at it now: act on it, hold it again, or tell the owner."
}
