package tasks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
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
//
// A send the ledger failed as never sent (sessions.NotSent: kagent refused it, for one, while the
// recipient could not take input) is safe to repeat and usually means the recipient is busy, so it
// may be sent up to maxNotSentSends times, each after [Limits.ResendBackoff] from the failure. With
// the default backoff that is about half an hour of trying before the reporting task is blocked.
const (
	maxSends        = 3
	maxNotSentSends = 10
)

var defaultResendBackoff = reconcile.Backoff{Base: 30 * time.Second, Max: 5 * time.Minute}

func (s *Service) resendBackoff() reconcile.Backoff {
	if s.Limits.ResendBackoff == (reconcile.Backoff{}) {
		return defaultResendBackoff
	}
	return s.Limits.ResendBackoff
}

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
	if n := len(r.ScreenshotIds); n > 0 {
		text += fmt.Sprintf("\nScreenshots: %d (in the task view)", n)
	}
	return text
}

// prepareBatch writes the delivery's target and message id, and in the same transaction gives that
// message id to the other pending reports for the same recipient that have no message yet. Each
// keeps its own row; only the message is shared. When this row was already taken into another
// batch, or the lease was lost, it returns the delivery without a message id.
func (s *Service) prepareBatch(ctx context.Context, c reconcile.Claim, d dbq.TaskEventDelivery, attemptID *string) (dbq.TaskEventDelivery, error) {
	mid := messageID(d.ID, d.Sends+1)
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		// One batch per recipient at a time, so two deliveries prepared together cannot each leave
		// the other out: the second waits, then finds its row already in the first one's message.
		if err := db.XactLock(ctx, tx, "mainloop:task-report-batch:"+d.RecipientKey); err != nil {
			return err
		}
		q := dbq.New(tx)
		prepared, err := q.PrepareTaskEventDelivery(ctx, dbq.PrepareTaskEventDeliveryParams{
			ID: d.ID, LeaseOwner: &c.Owner, Attempts: int32(c.Attempts), //nolint:gosec // a small counter
			TargetAttemptID: attemptID, DeliveryID: &mid,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		d = prepared
		_, err = q.JoinTaskEventBatch(ctx, dbq.JoinTaskEventBatchParams{
			TargetAttemptID: attemptID, DeliveryID: &mid, RecipientKey: d.RecipientKey, LeaderID: d.ID,
		})
		return err
	})
	return d, err
}

// batchText is the message for a ledger message id: the text of each report and loop firing it
// carries, oldest first. It is read from rows that do not change once they have a message id, not
// from the claim, so whichever claimer sends the message sends the same text.
// batchText is the text of the message that delivers batch mid, and the reports in it that show
// screenshots.
func (s *Service) batchText(ctx context.Context, mid string) (string, []reportShots, error) {
	q := dbq.New(s.Pool)
	ds, err := q.ListTaskEventBatch(ctx, &mid)
	if err != nil {
		return "", nil, err
	}
	parts := make([]string, 0, len(ds)+1)
	reports := 0
	var shots []reportShots
	for _, d := range ds {
		var text string
		switch {
		case d.ReportID != nil:
			reports++
			var ids []string
			text, ids, err = s.reportItemText(ctx, q, *d.ReportID)
			if len(ids) > 0 {
				shots = append(shots, reportShots{reportID: *d.ReportID, ids: ids})
			}
		case d.NoticeTaskID != nil:
			var t dbq.Task
			if t, err = q.GetTaskByID(ctx, *d.NoticeTaskID); err == nil {
				text = noticeText(t)
			}
		default:
			text, err = s.firingText(ctx, q, deref(d.LoopFiringID))
		}
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, text)
	}
	switch {
	case len(parts) == 1:
		return parts[0], shots, nil
	case reports == len(parts):
		parts = append([]string{fmt.Sprintf("[%d reports]", len(parts))}, parts...)
	default:
		parts = append([]string{fmt.Sprintf("[%d updates]", len(parts))}, parts...)
	}
	return strings.Join(parts, "\n\n"), shots, nil
}

// reportShots is a report of a batch and the screenshots it shows.
type reportShots struct {
	reportID string
	ids      []string
}

// reportImages copies the screenshots of the batch's reports into chat images of the main
// conversation and returns their ids, at most sessions.MaxImagesPerMessage, in report order. The
// ids derive from the message, so a replay of the same send finds the images it copied before.
func (s *Service) reportImages(ctx context.Context, mid, mainID string, shots []reportShots) ([]string, error) {
	if len(shots) == 0 {
		return nil, nil
	}
	reportIDs := make([]string, 0, len(shots))
	var images []string
	for _, r := range shots {
		reportIDs = append(reportIDs, r.reportID)
		for _, id := range r.ids {
			if len(images) < sessions.MaxImagesPerMessage {
				images = append(images, mid+":"+id)
			}
		}
	}
	q := dbq.New(s.Pool)
	err := q.CopyReportScreenshotsToMessage(ctx, dbq.CopyReportScreenshotsToMessageParams{
		MessageID: mid, SessionID: mainID, ReportIds: reportIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("copy report screenshots: %w", err)
	}
	// A screenshot whose image expired before the report went out has nothing to copy; the message
	// carries the rest.
	held, err := q.ListMessageImageIDs(ctx, dbq.ListMessageImageIDsParams{SessionID: mainID, Ids: images})
	if err != nil {
		return nil, fmt.Errorf("copy report screenshots: %w", err)
	}
	return slices.DeleteFunc(images, func(id string) bool { return !slices.Contains(held, id) }), nil
}

func (s *Service) reportItemText(ctx context.Context, q *dbq.Queries, reportID string) (string, []string, error) {
	r, err := q.GetTaskReport(ctx, reportID)
	if err != nil {
		return "", nil, err
	}
	t, err := q.GetTaskByID(ctx, r.TaskID)
	if err != nil {
		return "", nil, err
	}
	a, err := q.GetTaskAttempt(ctx, r.AttemptID)
	if err != nil {
		return "", nil, err
	}
	return reportText(r, t, a.Number), r.ScreenshotIds, nil
}

// readyAt is when a pending delivery may first be sent: a report after the batch window, a loop
// firing after the window and its minimum interval.
func (s *Service) readyAt(ctx context.Context, q *dbq.Queries, d dbq.TaskEventDelivery) (time.Time, error) {
	if d.ReportID != nil {
		r, err := q.GetTaskReport(ctx, *d.ReportID)
		if err != nil {
			return time.Time{}, err
		}
		return r.CreatedAt.Add(s.Limits.ReportBatchWindow), nil
	}
	if d.NoticeTaskID != nil {
		t, err := q.GetTaskByID(ctx, *d.NoticeTaskID)
		if err != nil {
			return time.Time{}, err
		}
		return t.CreatedAt.Add(s.Limits.ReportBatchWindow), nil
	}
	f, err := q.GetLoopFiring(ctx, deref(d.LoopFiringID))
	if err != nil {
		return time.Time{}, err
	}
	at := f.CreatedAt.Add(s.Limits.ReportBatchWindow)
	if f.NotBefore.After(at) {
		at = f.NotBefore
	}
	return at, nil
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
	if TaskFinal(p.Status) {
		return nil, "", true, nil
	}
	if p.CurrentAttemptID == nil {
		// A standing supervisor with no attempt yet is being started (an owner's task can start it):
		// its first attempt gets the delivery. Another parent with none has ended.
		return nil, "", !p.Standing, nil
	}
	a, err := q.GetTaskAttempt(ctx, *p.CurrentAttemptID)
	if err != nil {
		return nil, "", false, err
	}
	if a.State != dbstate.AttemptActive || a.SessionID == nil {
		// A parent still being created gets its report once it is active; the others have ended,
		// except a standing supervisor's, which a restart is replacing: its successor gets it.
		return nil, "", a.State != dbstate.AttemptCreating && !p.Standing, nil
	}
	if p.Standing && a.BriefDeliveryID == nil {
		// A standing supervisor's attempt is active, and has its session, a moment before its first
		// brief is recorded: nothing reaches it before the brief, which says what it is for.
		return nil, "", false, nil
	}
	return &a.ID, *a.SessionID, false, nil
}

// sendReport records the report in the sessions ledger. The target and message id are written with
// the claim before the send, and the ledger is read first on a replay, so a crash after the send
// never sends the report twice.
func (s *Service) sendReport(ctx context.Context, c reconcile.Claim, d dbq.TaskEventDelivery) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
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
		// A report waits while its recipient has an open turn, and for the batch window after it was
		// made, so the reports that pile up meanwhile go out as one message: one turn, not one each.
		// It is never steered in; it waits for the turn to end. A loop firing waits the same way,
		// and for its minimum interval.
		ready, err := s.readyAt(ctx, q, d)
		if err != nil {
			return reconcile.Outcome{}, err
		}
		if wait := ready.Sub(s.now()); wait > 0 {
			return reconcile.RetryAfter(wait), nil
		}
		busy, err := s.Sessions.HasOpen(ctx, sessionID)
		if err != nil {
			return reconcile.Outcome{}, err
		}
		if busy {
			return reconcile.RetryAfter(2 * time.Second), nil
		}
		if d, err = s.prepareBatch(ctx, c, d, attemptID); err != nil {
			return reconcile.Outcome{}, err
		}
		if d.DeliveryID == nil {
			// Another delivery's batch took this row (or the lease was lost): look again shortly,
			// when it has the shared message id to follow.
			return reconcile.RetryAfter(10 * time.Millisecond), nil
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
		text, shots, err := s.batchText(ctx, mid)
		if err != nil {
			return reconcile.Outcome{}, err
		}
		if d.RecipientKey == "main" {
			var images []string
			if images, err = s.reportImages(ctx, mid, sessionID, shots); err != nil {
				return reconcile.Outcome{}, err
			}
			_, err = s.Sessions.SendAsWithImages(ctx, sessions.SourceMainloop, mid, "", text, images)
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
// failed one is recorded again under a new id, up to maxSends times, or maxNotSentSends with a
// backoff when the ledger says it was never sent; after that the delivery is uncertain, the failure
// is written to the reporting attempt's evidence and its task is blocked.
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
		limit := maxSends
		notSent := sessions.NotSent(row.Detail)
		if notSent {
			limit = maxNotSentSends
		}
		if int(d.Sends) < limit {
			if notSent {
				// Never sent, most often because the recipient could not take input yet: wait before
				// asking again, counted from when the ledger gave up on the last send.
				if wait := row.UpdatedAt.Add(s.resendBackoff().Delay(int(d.Sends))).Sub(s.now()); wait > 0 {
					return reconcile.RetryAfter(wait), nil
				}
			}
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
	if d.ReportID == nil {
		// A loop firing or a notice has no report to block on: the delivery ends uncertain, which
		// the loop's firings show.
		return s.setDelivery(ctx, c, dbstate.TaskDeliveryUncertain)
	}
	note := fmt.Sprintf("native-delivery-failed:%s: %s", row.ID, detail)
	err := s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		r, err := q.GetTaskReport(ctx, *d.ReportID)
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
