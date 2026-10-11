package health

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// RecoverSweep is the collector as a reconcile sweep. Every delivery that carries a kagent task,
// running or finished, becomes a receipt row (state pending), and each pending receipt is read from
// kagent and stored; a running turn is read again until it ends. The receipt rows are the durable queue: a restart, a crash or a lost read leaves the row
// pending, and the next pass picks it up. Only one process runs a pass at a time (the runner holds
// the sweep's advisory lock).
func (s *Service) RecoverSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "health-recovery", Every: orDefault(s.RecoverEvery, defaultRecoverEvery), Run: s.Recover}
}

// RetainSweep is the retention pass as a reconcile sweep.
func (s *Service) RetainSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "health-retention", Every: orDefault(s.RetainEvery, defaultRetainEvery), Run: func(ctx context.Context) error {
		_, err := s.Retain(ctx)
		return err
	}}
}

// Recover runs one collection pass: it starts tracking new receipts, gives up on receipts whose
// session is gone, and reads the receipts that are due. No transaction is open while kagent is
// read; each task is stored in its own short transaction afterwards.
func (s *Service) Recover(ctx context.Context) error {
	if s.Kagent == nil || s.Agents == nil {
		return errors.New("health: recovery needs kagent and an agent resolver")
	}
	q := dbq.New(s.Pool)
	since := s.now().Add(-s.EventRetention)
	if _, err := q.DiscoverHealthReceipts(ctx, dbq.DiscoverHealthReceiptsParams{
		Gateway: s.Gateway, OwnerID: s.OwnerID, Since: since, MaxRows: discoverBatch,
	}); err != nil {
		return fmt.Errorf("health: discover receipts: %w", err)
	}
	if _, err := q.CloseStaleHealthReceipts(ctx, dbq.CloseStaleHealthReceiptsParams{OwnerID: s.OwnerID, Gateway: s.Gateway, Since: since}); err != nil {
		return fmt.Errorf("health: close stale receipts: %w", err)
	}
	if _, err := q.AbandonOrphanHealthReceipts(ctx, dbq.AbandonOrphanHealthReceiptsParams{OwnerID: s.OwnerID, Gateway: s.Gateway}); err != nil {
		return fmt.Errorf("health: abandon receipts: %w", err)
	}
	// The clock is read after discovery: a receipt written a moment ago is due now.
	now := s.now()
	due, err := q.ListDueHealthReceipts(ctx, dbq.ListDueHealthReceiptsParams{
		OwnerID: s.OwnerID, Gateway: s.Gateway, Now: now, Since: since, MaxRows: clamp32(orDefault(s.Batch, defaultBatch)),
	})
	if err != nil {
		return fmt.Errorf("health: list due receipts: %w", err)
	}
	for _, r := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.collect(ctx, r); err != nil {
			s.log().Warn("health receipt not collected", "session", r.SessionID, "task", r.A2aTaskID, "attempt", r.Attempts+1, "error", err)
			s.fail(ctx, r)
		}
	}
	return nil
}

// collect reads one receipt's task and stores it. An error leaves the receipt for fail to record.
func (s *Service) collect(ctx context.Context, r dbq.ListDueHealthReceiptsRow) error {
	agent, err := s.Agents.ResolveAgent(r.WorkspaceAgentNamespace, r.WorkspaceAgentName, &r.Kind, r.Role)
	if err != nil {
		return fmt.Errorf("resolve agent: %w", err)
	}
	task, err := s.getTask(ctx, agent, r.A2aTaskID)
	if err != nil {
		return fmt.Errorf("read task: %w", err)
	}
	runtimeSession := ""
	if r.RuntimeSessionID != nil {
		runtimeSession = *r.RuntimeSessionID
	}
	in := Input{
		OwnerID: s.OwnerID, Gateway: s.Gateway, SessionID: r.SessionID, DeliveryID: r.DeliveryID, ReceiptAt: r.ReceiptAt,
		TaskID: r.A2aTaskID, ContextID: task.ContextID, RuntimeSessionID: runtimeSession, Artifacts: task.Artifacts,
		Now: s.now(), EventRetention: s.EventRetention,
		Open: !r.DeliveryFinal, RereadAfter: orDefault(s.FirstBackoff, defaultFirstBackoff),
	}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		_, err := Ingest(ctx, tx, in)
		return err
	})
	if errors.Is(err, ErrSessionGone) {
		return nil // the orphan pass closes the receipt
	}
	return err
}

func (s *Service) getTask(ctx context.Context, agent kagent.AgentRef, taskID string) (*a2a.Task, error) {
	ctx, cancel := context.WithTimeout(ctx, orDefault(s.CollectTimeout, defaultCollectTimeout))
	defer cancel()
	return s.Kagent.GetTask(ctx, agent, taskID)
}

// fail records a failed read: the receipt stays pending with a later retry, or becomes unavailable
// once it has had its attempts. Both show as the source_unavailable gap.
func (s *Service) fail(ctx context.Context, r dbq.ListDueHealthReceiptsRow) {
	delay := orDefault(s.FirstBackoff, defaultFirstBackoff)
	for i := int32(0); i < r.Attempts && delay < maxBackoff; i++ {
		delay *= 2
	}
	delay = min(delay, maxBackoff)
	err := dbq.New(s.Pool).FailHealthReceipt(ctx, dbq.FailHealthReceiptParams{
		OwnerID: s.OwnerID, Gateway: s.Gateway, SessionID: r.SessionID, A2aTaskID: r.A2aTaskID,
		MaxAttempts: clamp32(orDefault(s.MaxAttempts, defaultMaxAttempts)), NextAttemptAt: s.now().Add(delay),
	})
	if err != nil {
		s.log().Warn("health receipt failure not recorded", "session", r.SessionID, "task", r.A2aTaskID, "error", err)
	}
}

// RetainResult counts what a retention pass changed.
type RetainResult struct {
	Frozen         int64
	EventsDeleted  int64
	TurnsDeleted   int64
	ReceiptsPruned int64
}

// Retain applies the retention windows. A turn that has an observation past the event window is
// frozen first (its rollup stays as it is and it takes no more observations), then the
// observations past the window are deleted; rollups and receipts go after the longer summary
// window. Nothing inside its window is touched. It is one transaction, so a failure changes
// nothing, and running it again is harmless.
func (s *Service) Retain(ctx context.Context) (RetainResult, error) {
	now := s.now()
	eventCutoff := now.Add(-s.EventRetention)
	summaryCutoff := now.Add(-s.SummaryRetention)
	var res RetainResult
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		var err error
		if res.Frozen, err = q.FreezeHealthTurns(ctx, dbq.FreezeHealthTurnsParams{OwnerID: s.OwnerID, Cutoff: eventCutoff}); err != nil {
			return err
		}
		if res.EventsDeleted, err = q.DeleteExpiredHealthEvents(ctx, dbq.DeleteExpiredHealthEventsParams{OwnerID: s.OwnerID, Cutoff: eventCutoff}); err != nil {
			return err
		}
		if res.TurnsDeleted, err = q.DeleteExpiredHealthTurns(ctx, dbq.DeleteExpiredHealthTurnsParams{OwnerID: s.OwnerID, Cutoff: summaryCutoff}); err != nil {
			return err
		}
		res.ReceiptsPruned, err = q.DeleteExpiredHealthReceipts(ctx, dbq.DeleteExpiredHealthReceiptsParams{OwnerID: s.OwnerID, Cutoff: summaryCutoff})
		return err
	})
	if err != nil {
		return RetainResult{}, fmt.Errorf("health: retention: %w", err)
	}
	return res, nil
}

// clamp32 narrows a configured count for a query parameter.
func clamp32(n int) int32 {
	return int32(min(n, math.MaxInt32)) //nolint:gosec // bounded by the min
}
