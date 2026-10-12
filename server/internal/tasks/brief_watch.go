package tasks

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// evidenceBriefFailed is written to an attempt when its brief could not be delivered in maxSends
// sends, or its session ended before a send could be made; the task is then blocked for
// reconciliation.
const evidenceBriefFailed = "brief-delivery-failed"

// BriefSweep watches the first brief of active attempts like a report: a ledger row that ended failed
// is sent again under the next derived id, up to maxSends sends. After that, or when the session has
// ended, the attempt gets evidence and its task is blocked for reconciliation. A row that ended
// cancelled is the owner stopping the turn or the session: the owner is steering the child, so the
// brief is not sent again.
func (s *Service) BriefSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "task-briefs", Every: s.Limits.Interval, Run: s.runBriefs}
}

func (s *Service) runBriefs(ctx context.Context) error {
	rows, err := dbq.New(s.Pool).ListFailedBriefAttempts(ctx, 50)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range rows {
		if err := s.resendBrief(ctx, a); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// resendBrief finds the first send of the brief that the ledger did not end failed and makes it the
// attempt's brief; if there is none and sends are left, it records the next. Ids are derived and the
// ledger is read first, so a crash never sends a brief twice.
func (s *Service) resendBrief(ctx context.Context, a dbq.TaskAttempt) error {
	q := dbq.New(s.Pool)
	if a.SessionID == nil {
		return nil
	}
	last := ""
	for n := range maxSends {
		id := briefSendID(a.ID, n)
		row, err := q.GetDelivery(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return s.sendNextBrief(ctx, a, id)
		}
		if err != nil {
			return err
		}
		if row.State != dbstate.DeliveryFailed {
			if a.BriefDeliveryID != nil && *a.BriefDeliveryID == id {
				return nil
			}
			return q.SetAttemptBrief(ctx, dbq.SetAttemptBriefParams{ID: a.ID, BriefDeliveryID: &id})
		}
		last = id
	}
	return s.giveUpBrief(ctx, a.ID, last)
}

// sendNextBrief records the next send of the brief. A session that has ended or been archived can never
// take it, so that gives up; a busy session or one whose workspace is not ready is tried again on the
// next pass.
func (s *Service) sendNextBrief(ctx context.Context, a dbq.TaskAttempt, id string) error {
	q := dbq.New(s.Pool)
	t, err := q.GetTaskByID(ctx, a.TaskID)
	if err != nil {
		return err
	}
	_, err = s.Sessions.SendToAs(ctx, sessions.SourceMainloop, *a.SessionID, id, DelegatedBrief(a.Role, t.Mode, t.Brief))
	switch {
	case err == nil:
		return q.SetAttemptBrief(ctx, dbq.SetAttemptBriefParams{ID: a.ID, BriefDeliveryID: &id})
	case errors.Is(err, sessions.ErrBusy):
		return nil // the next pass tries again
	case errors.Is(err, sessions.ErrNotFound):
		return s.giveUpBrief(ctx, a.ID, id)
	case errors.Is(err, sessions.ErrConflict):
		ended, oerr := s.sessionEnded(ctx, t.OwnerID, *a.SessionID)
		if oerr != nil {
			return oerr
		}
		if ended {
			return s.giveUpBrief(ctx, a.ID, id)
		}
		return nil // the workspace is not ready yet
	}
	return err
}

// sessionEnded is whether the session can never take a message again: it is gone, cancelled, failed or
// archived.
func (s *Service) sessionEnded(ctx context.Context, ownerID, sessionID string) (bool, error) {
	b, err := dbq.New(s.Pool).GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: ownerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return b.ArchivedAt != nil || b.SessionStatus == dbstate.SessionCancelled || b.SessionStatus == dbstate.SessionFailed, nil
}

// giveUpBrief records that the brief could not be delivered and blocks the task, once.
func (s *Service) giveUpBrief(ctx context.Context, attemptID, lastID string) error {
	return s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		a, err := q.GetTaskAttempt(ctx, attemptID)
		if err != nil {
			return nil, err
		}
		t, err := q.GetTaskByIDForUpdate(ctx, a.TaskID)
		if err != nil {
			return nil, err
		}
		for _, e := range a.EvidenceRefs {
			if e == evidenceBriefFailed {
				return nil, nil
			}
		}
		if err := q.AppendAttemptEvidence(ctx, dbq.AppendAttemptEvidenceParams{ID: a.ID, Column2: evidenceBriefFailed}); err != nil {
			return nil, err
		}
		if err := q.AppendAttemptEvidence(ctx, dbq.AppendAttemptEvidenceParams{ID: a.ID, Column2: "brief-delivery-failed:" + lastID}); err != nil {
			return nil, err
		}
		return s.blockReconciliation(ctx, q, t)
	})
}
