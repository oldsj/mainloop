// Package devloop lets a task agent iterate on the shared mainloop-dev instance the way `make
// dev-live` does: change code, sync, read logs, run a smoke check. It never gives a workspace
// Kubernetes credentials. The MCP listener (serve-mcp) only reads and writes durable rows in this
// package; serve-api's reconcile loop is the only caller that holds a Kubernetes client, mirroring
// the boundary serve-mcp already keeps for credential Secrets. See docs/specs/devloop.md.
package devloop

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// LeaseID is the one lease this package manages: mainloop-dev has exactly one live-sync target.
const LeaseID = "mainloop-dev"

// DefaultIdleTimeout is how long a held lease may go unused before it is taken over.
const DefaultIdleTimeout = 20 * time.Minute

// QueueStaleAfter is how long a queued session may go without polling dev_lease_acquire before
// its place in line is dropped, so a waiter that left cannot be handed the lease. A waiter must
// call again at least this often to keep its place.
const QueueStaleAfter = 2 * time.Minute

func staleSecs() float64 { return QueueStaleAfter.Seconds() }

// LeaseStatus is what [LeaseService.Acquire] reports.
type LeaseStatus string

// Lease statuses.
const (
	LeaseAcquired LeaseStatus = "acquired"
	LeaseQueued   LeaseStatus = "queued"
)

// LeaseInfo is the current state of the lease, for a caller's own status or for the task view.
type LeaseInfo struct {
	Status        LeaseStatus
	HolderSession string
	AcquiredAt    *time.Time
	LastUsedAt    *time.Time
	IdleTimeout   time.Duration
	// Position is this caller's 1-based place in the FIFO queue; 0 when acquired.
	Position    int
	QueueLength int
}

// LeaseService holds the one mainloop-dev lease: a durable row, acquired_at, last_used_at, an
// idle timeout, and a FIFO queue. Every operation is DB-only; this package never opens a
// transaction around network or Kubernetes I/O.
type LeaseService struct {
	Pool *pgxpool.Pool
}

// Acquire tries to take the lease for sessionID. An idle-expired lease is taken over by whichever
// queued session is earliest, so a caller that is not yet first in line is told it is queued
// rather than granted the lease out of turn. Calling it again (a heartbeat, or a poll while
// queued) is always safe: a holder's call just extends last_used_at.
func (s *LeaseService) Acquire(ctx context.Context, sessionID string) (LeaseInfo, error) {
	var info LeaseInfo
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := q.UpsertDevloopLeaseQueueEntry(ctx, dbq.UpsertDevloopLeaseQueueEntryParams{
			LeaseID: LeaseID, SessionID: sessionID,
		}); err != nil {
			return err
		}
		lease, err := q.LockDevloopLease(ctx, LeaseID)
		if err != nil {
			return err
		}
		if err := q.DeleteStaleDevloopLeaseQueueEntries(ctx, dbq.DeleteStaleDevloopLeaseQueueEntriesParams{
			LeaseID: LeaseID, StaleSecs: staleSecs(),
		}); err != nil {
			return err
		}
		idle := time.Duration(lease.IdleTimeoutSeconds) * time.Second
		expired := lease.HolderSessionID == nil || lease.LastUsedAt == nil ||
			lease.LastUsedAt.Add(idle).Before(time.Now())
		if expired {
			next, err := q.NextDevloopLeaseQueueEntry(ctx, dbq.NextDevloopLeaseQueueEntryParams{LeaseID: LeaseID, StaleSecs: staleSecs()})
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				// No one is waiting; the lease stays free until someone asks.
			case err != nil:
				return err
			default:
				granted, err := q.GrantDevloopLease(ctx, dbq.GrantDevloopLeaseParams{
					ID: LeaseID, HolderSessionID: &next,
				})
				if err != nil {
					return err
				}
				if err := q.DequeueDevloopLeaseSession(ctx, dbq.DequeueDevloopLeaseSessionParams{
					LeaseID: LeaseID, SessionID: next,
				}); err != nil {
					return err
				}
				lease.HolderSessionID = &next
				lease.AcquiredAt, lease.LastUsedAt = granted.AcquiredAt, granted.LastUsedAt
			}
		}
		info.IdleTimeout = idle
		if lease.HolderSessionID != nil && *lease.HolderSessionID == sessionID {
			lastUsed, err := q.TouchDevloopLease(ctx, dbq.TouchDevloopLeaseParams{ID: LeaseID, HolderSessionID: &sessionID})
			if err != nil {
				return err
			}
			if err := q.DequeueDevloopLeaseSession(ctx, dbq.DequeueDevloopLeaseSessionParams{LeaseID: LeaseID, SessionID: sessionID}); err != nil {
				return err
			}
			info.Status, info.HolderSession = LeaseAcquired, sessionID
			info.AcquiredAt, info.LastUsedAt = lease.AcquiredAt, lastUsed
			return nil
		}
		info.Status = LeaseQueued
		if lease.HolderSessionID != nil {
			info.HolderSession = *lease.HolderSessionID
			info.AcquiredAt, info.LastUsedAt = lease.AcquiredAt, lease.LastUsedAt
		}
		pos, err := q.DevloopLeaseQueuePosition(ctx, dbq.DevloopLeaseQueuePositionParams{LeaseID: LeaseID, SessionID: sessionID})
		if err != nil {
			return err
		}
		length, err := q.DevloopLeaseQueueLength(ctx, dbq.DevloopLeaseQueueLengthParams{LeaseID: LeaseID, StaleSecs: staleSecs()})
		if err != nil {
			return err
		}
		info.Position, info.QueueLength = int(pos)+1, int(length)
		return nil
	})
	if err != nil {
		return LeaseInfo{}, err
	}
	return info, nil
}

// Release gives up the lease if sessionID holds it, and drops its queue entry if it was only
// waiting. It is a no-op, not an error, for a session that holds neither.
func (s *LeaseService) Release(ctx context.Context, sessionID string) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if _, err := q.ReleaseDevloopLease(ctx, dbq.ReleaseDevloopLeaseParams{ID: LeaseID, HolderSessionID: &sessionID}); err != nil {
			return err
		}
		return q.DequeueDevloopLeaseSession(ctx, dbq.DequeueDevloopLeaseSessionParams{LeaseID: LeaseID, SessionID: sessionID})
	})
}

// Status reads the lease without taking it or joining the queue, for the owner-facing task view.
func (s *LeaseService) Status(ctx context.Context) (LeaseInfo, error) {
	q := dbq.New(s.Pool)
	lease, err := q.GetDevloopLease(ctx, LeaseID)
	if err != nil {
		return LeaseInfo{}, err
	}
	length, err := q.DevloopLeaseQueueLength(ctx, dbq.DevloopLeaseQueueLengthParams{LeaseID: LeaseID, StaleSecs: staleSecs()})
	if err != nil {
		return LeaseInfo{}, err
	}
	info := LeaseInfo{IdleTimeout: time.Duration(lease.IdleTimeoutSeconds) * time.Second, QueueLength: int(length)}
	if lease.HolderSessionID != nil {
		info.Status, info.HolderSession = LeaseAcquired, *lease.HolderSessionID
		info.AcquiredAt, info.LastUsedAt = lease.AcquiredAt, lease.LastUsedAt
	}
	return info, nil
}

// ErrNotHolder means the session does not hold the lease, or held it but let it idle-expire with
// another session waiting.
var ErrNotHolder = errors.New("devloop: this session does not hold the mainloop-dev lease")

// holds applies the one rule for "this session still holds the lease": it is the recorded holder,
// and either its idle timeout has not run out or nobody else is waiting for the lease (a live
// queued session other than itself takes it over once it has).
func holds(ctx context.Context, q *dbq.Queries, lease dbq.DevloopLease, sessionID string) (bool, error) {
	if lease.HolderSessionID == nil || *lease.HolderSessionID != sessionID || lease.LastUsedAt == nil {
		return false, nil
	}
	if !lease.LastUsedAt.Add(time.Duration(lease.IdleTimeoutSeconds) * time.Second).Before(time.Now()) {
		return true, nil
	}
	next, err := q.NextDevloopLeaseQueueEntry(ctx, dbq.NextDevloopLeaseQueueEntryParams{LeaseID: LeaseID, StaleSecs: staleSecs()})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return true, nil
	case err != nil:
		return false, err
	default:
		return next == sessionID, nil
	}
}

// Use is a holder's heartbeat for work other than dev_lease_acquire: it extends last_used_at so an
// active sync loop does not idle-expire, and fails with [ErrNotHolder] when the session no longer
// holds the lease (including an expired lease a queued session is next in line for).
func (s *LeaseService) Use(ctx context.Context, sessionID string) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		lease, err := q.LockDevloopLease(ctx, LeaseID)
		if err != nil {
			return err
		}
		if ok, err := holds(ctx, q, lease, sessionID); err != nil {
			return err
		} else if !ok {
			return ErrNotHolder
		}
		_, err = q.TouchDevloopLease(ctx, dbq.TouchDevloopLeaseParams{ID: LeaseID, HolderSessionID: &sessionID})
		return err
	})
}

// Holds reports whether sessionID still holds the lease by the same rule as [LeaseService.Use],
// without touching it.
func (s *LeaseService) Holds(ctx context.Context, sessionID string) (bool, error) {
	q := dbq.New(s.Pool)
	lease, err := q.GetDevloopLease(ctx, LeaseID)
	if err != nil {
		return false, err
	}
	return holds(ctx, q, lease, sessionID)
}
