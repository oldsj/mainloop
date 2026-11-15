package tasks

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// The owner's view and controls of loops (docs/specs/loops.md, "The owner's view and controls"):
// the task page lists a task's loops, and the owner pauses, resumes, fires or deletes one. Main and
// supervisors arm loops; the owner only steers the ones they armed.

const (
	// ReasonLoopState refuses a control the loop's state does not allow, such as pausing a loop
	// that is not armed.
	ReasonLoopState = "loop_state_conflict"
	// recentLoopFirings is how many firings each loop on the task page carries.
	recentLoopFirings = 5
)

// TaskLoops lists the loops on a task's page: those watching the task and those its supervisor
// armed, live ones first, each with its latest firings.
func (s *Service) TaskLoops(ctx context.Context, p Principal, taskID string) ([]api.TaskLoop, error) {
	q := dbq.New(s.Pool)
	if _, err := q.GetTask(ctx, dbq.GetTaskParams{ID: taskID, OwnerID: p.OwnerID}); err != nil {
		return nil, notFoundOr(err, ReasonTaskNotFound)
	}
	loops, err := q.ListLoopsOfTask(ctx, dbq.ListLoopsOfTaskParams{OwnerID: p.OwnerID, TaskID: taskID, MaxRows: maxListedLoops})
	if err != nil {
		return nil, err
	}
	return s.loopViews(ctx, q, loops)
}

func (s *Service) loopViews(ctx context.Context, q *dbq.Queries, loops []dbq.TaskLoop) ([]api.TaskLoop, error) {
	ids := make([]string, len(loops))
	for i, l := range loops {
		ids[i] = l.ID
	}
	firings, err := q.ListLatestLoopFirings(ctx, dbq.ListLatestLoopFiringsParams{LoopIds: ids, PerLoop: recentLoopFirings})
	if err != nil {
		return nil, err
	}
	byLoop := map[string][]api.LoopFiring{}
	for _, f := range firings {
		v := api.LoopFiring{
			Number: int(f.Number), Facts: f.Facts, Folded: int(f.Folded), CreatedAt: f.CreatedAt,
			DeliveryState: api.LoopFiringDeliveryState(f.DeliveryState),
		}
		if f.StopsLoop != nil {
			stops := api.LoopFiringStopsLoop(*f.StopsLoop)
			v.StopsLoop = &stops
		}
		byLoop[f.LoopID] = append(byLoop[f.LoopID], v)
	}
	out := make([]api.TaskLoop, len(loops))
	for i, l := range loops {
		out[i] = loopView(l, byLoop[l.ID])
	}
	return out, nil
}

// loopView is a loop as the contract shows it.
func loopView(l dbq.TaskLoop, firings []api.LoopFiring) api.TaskLoop {
	v := api.TaskLoop{
		Id: l.ID, State: api.TaskLoopState(l.State), Trigger: api.TaskLoopTrigger(l.Trigger), TaskId: l.SubjectTaskID,
		Recipient: api.TaskLoopRecipientMain, Note: l.Note, Statuses: l.Statuses,
		PrConditions: make([]api.TaskLoopPrConditions, len(l.PrConditions)), ExpectChange: l.ExpectChange,
		Firings: int(l.Firings), MaxFirings: int(l.MaxFirings), MinIntervalSeconds: int(l.MinIntervalSeconds),
		LastFiredAt: l.LastFiredAt, ExpiresAt: l.ExpiresAt, CreatedAt: l.CreatedAt, UpdatedAt: l.UpdatedAt,
		RecentFirings: firings,
	}
	if v.Statuses == nil {
		v.Statuses = []string{}
	}
	if v.RecentFirings == nil {
		v.RecentFirings = []api.LoopFiring{}
	}
	for i, c := range l.PrConditions {
		v.PrConditions[i] = api.TaskLoopPrConditions(c)
	}
	if parent, ok := strings.CutPrefix(l.RecipientKey, "parent-task:"); ok {
		v.Recipient, v.RecipientTaskId = api.TaskLoopRecipientSupervisor, &parent
	}
	if l.EverySeconds != nil {
		every := int(*l.EverySeconds) / 60
		v.EveryMinutes = &every
	}
	if l.QuietSeconds != nil {
		quiet := quietMinutes(l)
		v.QuietMinutes = &quiet
	}
	if l.StopReason != nil {
		reason := api.TaskLoopStopReason(*l.StopReason)
		v.StopReason = &reason
	}
	if l.State == dbstate.LoopArmed {
		switch {
		case l.Trigger == dbstate.LoopTriggerTime:
			v.NextFireAt = l.FireAt
		case l.Trigger == dbstate.LoopTriggerQuiet && l.QuietSince != nil:
			at := l.QuietSince.Add(time.Duration(quietMinutes(l)) * time.Minute)
			v.NextFireAt = &at
		}
	}
	return v
}

// PauseLoop pauses an armed loop of the owner's.
func (s *Service) PauseLoop(ctx context.Context, p Principal, loopID string) (api.TaskLoop, error) {
	return s.ownerLoopControl(ctx, p, loopID, func(q *dbq.Queries) (dbq.TaskLoop, error) {
		return q.PauseTaskLoop(ctx, dbq.PauseTaskLoopParams{ID: loopID, OwnerID: p.OwnerID})
	})
}

// ResumeLoop resumes a paused loop of the owner's; it looks at once.
func (s *Service) ResumeLoop(ctx context.Context, p Principal, loopID string) (api.TaskLoop, error) {
	return s.ownerLoopControl(ctx, p, loopID, func(q *dbq.Queries) (dbq.TaskLoop, error) {
		l, err := q.ResumeTaskLoop(ctx, dbq.ResumeTaskLoopParams{ID: loopID, OwnerID: p.OwnerID})
		if err == nil {
			err = s.notify(ctx, q, LoopChannel, l.ID)
		}
		return l, err
	})
}

// DeleteLoop stops a live loop of the owner's for good. A firing it already made is still
// delivered, and the loop stays in the task's history as stopped.
func (s *Service) DeleteLoop(ctx context.Context, p Principal, loopID string) (api.TaskLoop, error) {
	return s.ownerLoopControl(ctx, p, loopID, func(q *dbq.Queries) (dbq.TaskLoop, error) {
		return q.OwnerStopTaskLoop(ctx, dbq.OwnerStopTaskLoopParams{ID: loopID, OwnerID: p.OwnerID})
	})
}

// ownerLoopControl runs one state change of a loop of the owner's in a transaction. A change that
// matched nothing is 404 for a loop that is not the owner's, else 409 for its state.
func (s *Service) ownerLoopControl(ctx context.Context, p Principal, loopID string, change func(*dbq.Queries) (dbq.TaskLoop, error)) (api.TaskLoop, error) {
	var out api.TaskLoop
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		l, err := change(q)
		if errors.Is(err, pgx.ErrNoRows) {
			l, err = q.GetTaskLoopByID(ctx, loopID)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && l.OwnerID != p.OwnerID {
				return refusal(404, ReasonLoopNotFound)
			}
			if err != nil {
				return err
			}
			return refusal(409, ReasonLoopState)
		}
		if err != nil {
			return err
		}
		views, err := s.loopViews(ctx, q, []dbq.TaskLoop{l})
		if err != nil {
			return err
		}
		out = views[0]
		return nil
	})
	return out, err
}

// FireLoop fires a live loop of the owner's now: one firing, as its trigger would make, without
// waiting for the minimum interval, or folded into its firing still waiting to be sent. The same
// request id again fires nothing more.
func (s *Service) FireLoop(ctx context.Context, p Principal, loopID, requestID string) (api.TaskLoop, error) {
	if requestID == "" || len(requestID) > 100 {
		return api.TaskLoop{}, refusal(422, ReasonInvalidRequest)
	}
	var out api.TaskLoop
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		l, err := q.GetTaskLoopForUpdate(ctx, loopID)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && l.OwnerID != p.OwnerID {
			return refusal(404, ReasonLoopNotFound)
		}
		if err != nil {
			return err
		}
		if deref(l.FireRequestID) != requestID {
			if l.State != dbstate.LoopArmed && l.State != dbstate.LoopPaused {
				return refusal(409, ReasonLoopState)
			}
			e := loopEval{state: l.State, firings: l.Firings, lastAt: l.LastFiredAt, immediate: true}
			e.fact = "fired now by the owner at " + s.now().UTC().Format(time.RFC3339)
			if l.SubjectTaskID != nil {
				t, err := q.GetTaskByID(ctx, *l.SubjectTaskID)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				if err == nil {
					e.fact += "; " + taskFact(t, nil)
				}
			}
			if err := s.fire(ctx, q, l, &e); err != nil {
				return err
			}
			if l, err = q.SetTaskLoopFiredNow(ctx, dbq.SetTaskLoopFiredNowParams{
				ID: l.ID, Firings: e.firings, LastFiredAt: e.lastAt, State: e.state, FireRequestID: &requestID,
			}); err != nil {
				return err
			}
		}
		views, err := s.loopViews(ctx, q, []dbq.TaskLoop{l})
		if err != nil {
			return err
		}
		out = views[0]
		return nil
	})
	return out, err
}
