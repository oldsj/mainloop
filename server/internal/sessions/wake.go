package sessions

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// WakeTurn makes the delivery that follows a parked task due now and wakes the delivery loop, the
// way a new message does. The HITL dispatcher calls it once kagent has accepted the owner's answer:
// the delivery has been sleeping on its parked backoff, and the task is running again. A lost wake
// (a claimer held the delivery, or the notification was missed) only falls back to that backoff.
// It writes the database only, so it is safe to call after the kagent send.
func (s *Service) WakeTurn(ctx context.Context, kagentSessionID, taskID string) error {
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		ids, err := q.WakeParkedDeliveries(ctx, dbq.WakeParkedDeliveriesParams{KagentSessionID: &kagentSessionID, TaskID: &taskID})
		if err != nil {
			return err
		}
		for _, id := range ids {
			// Delivered to listeners when the transaction commits.
			if err := q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: DeliveryChannel, Payload: id}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("wake turn: %w", err)
	}
	return nil
}
