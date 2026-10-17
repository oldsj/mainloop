package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// EventChannel wakes the delivery loop; a report notifies it when it records deliveries.
const EventChannel = "mainloop_task_events"

// maxSends is how many times a report is recorded in the sessions ledger before the delivery is
// given up as uncertain. Each send uses a new message id, so a message the ledger failed is not
// mistaken for one that was delivered.
const maxSends = 3

// DeliveryTable is the table of report deliveries the loop drives.
func DeliveryTable() reconcile.Table {
	return reconcile.NewTable("task_event_deliveries",
		string(dbstate.TaskDeliveryPending), string(dbstate.TaskDeliveryQueued))
}

// DeliveryLoop sends reports to the parent task's session and to the main thread through the
// sessions delivery ledger, and follows each message until the ledger says it was delivered. It
// needs Sessions.
func (s *Service) DeliveryLoop() reconcile.Loop {
	return reconcile.Loop{
		Name: "task-event-deliveries", Table: DeliveryTable(), Handle: s.handleDelivery,
		Interval: s.Limits.Interval, Wake: []string{EventChannel},
	}
}

// messageID is the ledger id of the nth send of a delivery. It is derived, so a replay of a send
// whose bookkeeping was lost finds the ledger row the first run made.
func messageID(deliveryID string, send int32) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, fmt.Appendf(nil, "mainloop:task-event:%s:%d", deliveryID, send)).String()
}

func (s *Service) handleDelivery(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
	d, err := q.GetTaskEventDelivery(ctx, c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), nil
	}
	if err != nil {
		return reconcile.Outcome{}, err
	}
	var out reconcile.Outcome
	switch d.State {
	case dbstate.TaskDeliveryPending:
		out, err = s.sendReport(ctx, c, d)
	case dbstate.TaskDeliveryQueued:
		out, err = s.followReport(ctx, c, d)
	default:
		return reconcile.Recorded(), nil
	}
	if errors.Is(err, reconcile.ErrLeaseLost) {
		return reconcile.Recorded(), nil
	}
	return out, err
}

// reportText is what the recipient reads. A report is the child's own claim, so it says so.
func reportText(r dbq.TaskReport, t dbq.Task, attempt int32) string {
	who := "task"
	if t.ParentTaskID != nil {
		who = "child task"
	}
	text := fmt.Sprintf("[report from %s %s attempt %d; %s; unverified claim]\n%s", who, t.ID, attempt, r.Outcome, r.Summary)
	if r.OwnerAction != nil && *r.OwnerAction != "" {
		text += "\nOwner action needed: " + *r.OwnerAction
	}
	return text
}

// target resolves where a delivery goes. A parent whose attempt is no longer the active current one
// has ended; ended is true and the delivery is cancelled rather than retried.
func (s *Service) target(ctx context.Context, key string) (attemptID *string, sessionID string, ended bool, err error) {
	q := dbq.New(s.Pool)
	if key == "main" {
		m, err := s.Sessions.EnsureMain(ctx)
		return nil, m.SessionID, false, err
	}
	parentID, ok := strings.CutPrefix(key, "parent-task:")
	if !ok {
		return nil, "", true, nil
	}
	p, err := q.GetTaskByID(ctx, parentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", true, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	if TaskFinal(p.Status) || p.CurrentAttemptID == nil {
		return nil, "", true, nil
	}
	a, err := q.GetTaskAttempt(ctx, *p.CurrentAttemptID)
	if err != nil {
		return nil, "", false, err
	}
	if a.State != dbstate.AttemptActive || a.SessionID == nil {
		// A parent still being created gets its report once it is active; the others have ended.
		return nil, "", a.State != dbstate.AttemptCreating, nil
	}
	return &a.ID, *a.SessionID, false, nil
}

// sendReport records the report in the sessions ledger. The target and message id are written with
// the claim before the send, and the ledger is read first on a replay, so a crash after the send
// never sends the report twice.
func (s *Service) sendReport(ctx context.Context, c reconcile.Claim, d dbq.TaskEventDelivery) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
	r, err := q.GetTaskReport(ctx, d.ReportID)
	if err != nil {
		return reconcile.Outcome{}, err
	}
	t, err := q.GetTaskByID(ctx, r.TaskID)
	if err != nil {
		return reconcile.Outcome{}, err
	}
	a, err := q.GetTaskAttempt(ctx, r.AttemptID)
	if err != nil {
		return reconcile.Outcome{}, err
	}
	attemptID, sessionID, ended, err := s.target(ctx, d.RecipientKey)
	if err != nil {
		return reconcile.Outcome{}, err
	}
	if ended && d.DeliveryID == nil {
		return s.setDelivery(ctx, c, dbstate.TaskDeliveryCancelled)
	}
	if d.DeliveryID == nil {
		if sessionID == "" {
			return reconcile.RetryAfter(3 * time.Second), nil
		}
		mid := messageID(d.ID, d.Sends+1)
		d, err = q.PrepareTaskEventDelivery(ctx, dbq.PrepareTaskEventDeliveryParams{
			ID: d.ID, LeaseOwner: &c.Owner, Attempts: int32(c.Attempts), //nolint:gosec // a small counter
			TargetAttemptID: attemptID, DeliveryID: &mid,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return reconcile.Recorded(), nil
		}
		if err != nil {
			return reconcile.Outcome{}, err
		}
	}
	mid := *d.DeliveryID
	if sessionID == "" && d.TargetAttemptID != nil {
		// A replay after the parent stopped being the current attempt still sends to the session
		// it was prepared for, unless the ledger already has the message.
		ta, err := q.GetTaskAttempt(ctx, *d.TargetAttemptID)
		if err != nil {
			return reconcile.Outcome{}, err
		}
		sessionID = deref(ta.SessionID)
	}
	if _, err := q.GetDelivery(ctx, mid); errors.Is(err, pgx.ErrNoRows) {
		if ended {
			return s.setDelivery(ctx, c, dbstate.TaskDeliveryCancelled)
		}
		text := reportText(r, t, a.Number)
		if d.RecipientKey == "main" {
			_, err = s.Sessions.SendAs(ctx, sessions.SourceMainloop, mid, "", text)
		} else {
			_, err = s.Sessions.SendToAs(ctx, sessions.SourceMainloop, sessionID, mid, text)
		}
		switch {
		case errors.Is(err, sessions.ErrBusy):
			// A plain busy send now queues instead (see sessions.record): this only catches the rare
			// unique-constraint race busyOrErr maps to ErrBusy, so one more pass clears it.
			return reconcile.RetryAfter(3 * time.Second), nil
		case errors.Is(err, sessions.ErrConflict), errors.Is(err, sessions.ErrNotFound):
			// Not ready, or ended since the target was read: the next pass tells which.
			return reconcile.RetryAfter(5 * time.Second), nil
		case err != nil:
			return reconcile.Outcome{}, err
		}
	} else if err != nil {
		return reconcile.Outcome{}, err
	}
	return s.setDelivery(ctx, c, dbstate.TaskDeliveryQueued)
}

func (s *Service) setDelivery(ctx context.Context, c reconcile.Claim, to dbstate.TaskDeliveryState) (reconcile.Outcome, error) {
	_, err := dbq.New(s.Pool).SetTaskEventDeliveryState(ctx, dbq.SetTaskEventDeliveryStateParams{
		State: to, ID: c.ID, LeaseOwner: &c.Owner, Attempts: int32(c.Attempts), //nolint:gosec // a small counter
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), nil
	}
	if err != nil {
		return reconcile.Outcome{}, err
	}
	return reconcile.Recorded(), nil
}

// followReport watches the ledger row of a queued report. A delivered message ends the delivery. A
// failed one is recorded again under a new id, up to maxSends times; after that the delivery is
// uncertain, the failure is written to the reporting attempt's evidence and its task is blocked.
func (s *Service) followReport(ctx context.Context, c reconcile.Claim, d dbq.TaskEventDelivery) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
	if d.DeliveryID == nil {
		return s.setDelivery(ctx, c, dbstate.TaskDeliveryPending)
	}
	row, err := q.GetDelivery(ctx, *d.DeliveryID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The ledger has no such message: send it again under the same id.
		return s.setDelivery(ctx, c, dbstate.TaskDeliveryPending)
	}
	if err != nil {
		return reconcile.Outcome{}, err
	}
	switch row.State {
	case dbstate.DeliveryDelivered, dbstate.DeliveryCompleted:
		return s.setDelivery(ctx, c, dbstate.TaskDeliveryDelivered)
	case dbstate.DeliveryQueued, dbstate.DeliveryRecorded, dbstate.DeliverySending:
		// Queued behind the target's open turn, or admitted and on its way: either way, not sent yet.
		// It is looked at again, not resent; the sessions ledger admits it on its own once its turn
		// comes, however long the wait.
		return reconcile.RetryAfter(2 * time.Second), nil
	case dbstate.DeliveryCancelled:
		// The owner stopped the parent's turn, or the parent's session ended: that is an owner act, so
		// the report is not sent again over it. The delivery ends cancelled and the child is not
		// blocked; the report stays on the child, readable through the task tools.
		return s.setDelivery(ctx, c, dbstate.TaskDeliveryCancelled)
	case dbstate.DeliveryFailed:
		if d.Sends < maxSends {
			next := messageID(d.ID, d.Sends+1)
			_, err := q.ResendTaskEventDelivery(ctx, dbq.ResendTaskEventDeliveryParams{
				ID: d.ID, LeaseOwner: &c.Owner, Attempts: int32(c.Attempts), DeliveryID: &next, //nolint:gosec // a small counter
			})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return reconcile.Outcome{}, err
			}
			return reconcile.Recorded(), nil
		}
	}
	return s.giveUp(ctx, c, d, row)
}

// giveUp ends a delivery the ledger could not complete. The recipient may or may not have read the
// message, so the delivery is uncertain, not failed, and the reporting task is blocked for review.
func (s *Service) giveUp(ctx context.Context, c reconcile.Claim, d dbq.TaskEventDelivery, row dbq.Delivery) (reconcile.Outcome, error) {
	detail := string(row.State)
	if row.Detail != nil {
		detail = *row.Detail
	}
	note := fmt.Sprintf("native-delivery-failed:%s: %s", row.ID, detail)
	err := s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		r, err := q.GetTaskReport(ctx, d.ReportID)
		if err != nil {
			return nil, err
		}
		t, err := q.GetTaskByIDForUpdate(ctx, r.TaskID)
		if err != nil {
			return nil, err
		}
		if err := q.AppendAttemptEvidence(ctx, dbq.AppendAttemptEvidenceParams{ID: r.AttemptID, Column2: note}); err != nil {
			return nil, err
		}
		drafts, err := s.blockReconciliation(ctx, q, t)
		if err != nil {
			return nil, err
		}
		if _, err := q.SetTaskEventDeliveryState(ctx, dbq.SetTaskEventDeliveryStateParams{
			State: dbstate.TaskDeliveryUncertain, ID: d.ID, LeaseOwner: &c.Owner, Attempts: int32(c.Attempts), //nolint:gosec // a small counter
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, reconcile.ErrLeaseLost
			}
			return nil, err
		}
		return drafts, nil
	})
	if errors.Is(err, reconcile.ErrLeaseLost) {
		return reconcile.Recorded(), nil
	}
	if err != nil {
		return reconcile.Outcome{}, err
	}
	return reconcile.Recorded(), nil
}
