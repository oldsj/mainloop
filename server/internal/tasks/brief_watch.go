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
// sends; the task is then blocked for reconciliation.
const evidenceBriefFailed = "brief-delivery-failed"

// BriefSweep watches the first brief of active attempts like a report: a ledger row that ended failed
// or cancelled is sent again under the next derived id, up to maxSends sends. After that the
// attempt gets evidence and its task is blocked for reconciliation.
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

// resendBrief finds the first send of the brief that the ledger did not end failed or cancelled and
// makes it the attempt's brief; if there is none and sends are left, it records the next. Ids are
// derived and the ledger is read first, so a crash never sends a brief twice.
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
			t, err := q.GetTaskByID(ctx, a.TaskID)
			if err != nil {
				return err
			}
			if _, err := s.Sessions.SendTo(ctx, *a.SessionID, id, DelegatedBrief(a.Role, t.Mode, t.Brief)); err != nil {
				if errors.Is(err, sessions.ErrBusy) || errors.Is(err, sessions.ErrConflict) {
					return nil // the next pass tries again
				}
				return err
			}
			return q.SetAttemptBrief(ctx, dbq.SetAttemptBriefParams{ID: a.ID, BriefDeliveryID: &id})
		}
		if err != nil {
			return err
		}
		if row.State != dbstate.DeliveryFailed && row.State != dbstate.DeliveryCancelled {
			if a.BriefDeliveryID != nil && *a.BriefDeliveryID == id {
				return nil
			}
			return q.SetAttemptBrief(ctx, dbq.SetAttemptBriefParams{ID: a.ID, BriefDeliveryID: &id})
		}
		last = id
	}
	return s.giveUpBrief(ctx, a.ID, last)
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
		if err := q.AppendAttemptEvidence(ctx, dbq.AppendAttemptEvidenceParams{ID: a.ID, Column2: evidenceBriefFailed}); err != nil {
			return nil, err
		}
		if err := q.AppendAttemptEvidence(ctx, dbq.AppendAttemptEvidenceParams{ID: a.ID, Column2: "brief-delivery-failed:" + lastID}); err != nil {
			return nil, err
		}
		return s.blockReconciliation(ctx, q, t)
	})
}
