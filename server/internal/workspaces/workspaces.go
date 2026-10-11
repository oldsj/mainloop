// Package workspaces runs the workspace lifecycle on kagent: create, suspend, resume, refresh and
// delete, the idle-suspend and archived-delete sweeps, and the orphan sweep.
//
// Every lifecycle change is a workspace_ops row (an intent with an explicit state and an
// idempotency key) that a reconcile loop drives. The API queues the row, wakes the loop and waits a
// bounded time for the outcome; all kagent calls run from the loop, so no request, transaction or
// lock is held across one. Before any retry the loop observes the remote side: CreateSession is
// idempotent on the workspace's stored request id, so a replay returns the Session an earlier,
// interrupted attempt made instead of a second one. The kagent Session is the source of truth for
// the observed state; Mainloop stores only the request (repo, ref, ports, idle timeout) and the
// binding to the Session.
package workspaces

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/providers"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// Kagent is the part of the kagent client the lifecycle uses. *kagent.Client satisfies it.
type Kagent interface {
	CreateSession(ctx context.Context, req *kagentapi.CreateSessionRequest) (*kagentapi.Session, error)
	GetSession(ctx context.Context, id string) (*kagentapi.Session, error)
	SuspendSession(ctx context.Context, id string) (*kagentapi.Session, error)
	ResumeSession(ctx context.Context, id string) (*kagentapi.Session, error)
	DeleteSession(ctx context.Context, id string) (*kagentapi.Session, error)
	ListSessions(ctx context.Context) ([]*kagentapi.Session, error)
}

// Deliveries is the message delivery ledger as the lifecycle needs it. The delivery slice supplies
// it; until then a nil Service.Deliveries means no turn is ever open and no delivery counts as
// activity.
type Deliveries interface {
	// HasOpen reports whether a message for the session is recorded, queued, sending or in flight.
	HasOpen(ctx context.Context, sessionID string) (bool, error)
	// LastActivity is the newest delivery update for the session, or the zero time.
	LastActivity(ctx context.Context, sessionID string) (time.Time, error)
}

// Service is the workspace lifecycle. Build it with all fields set (Deliveries and Now optional).
type Service struct {
	Pool       *pgxpool.Pool
	Kagent     Kagent
	Providers  *providers.Registry
	Config     *config.Config
	Log        *slog.Logger
	OwnerID    string
	Deliveries Deliveries
	// Now is the clock for retry-window decisions; nil means time.Now.
	Now func() time.Time
	// PollEvery is how often a waiting request re-reads its operation; zero means 50ms.
	PollEvery time.Duration
}

// Errors the API maps to statuses.
var (
	// ErrNotFound means no such workspace for the owner.
	ErrNotFound = errors.New("workspace not found")
	// ErrBusy means another kind of operation is already open on the workspace.
	ErrBusy = errors.New("another workspace operation is in progress")
)

// RejectedError means the operation was refused for good: the workspace is not in a state it
// applies to, or kagent rejected it.
type RejectedError struct{ Detail string }

func (e *RejectedError) Error() string { return e.Detail }

// UnconfirmedError means kagent has not confirmed the operation; Mainloop keeps (or kept) trying.
type UnconfirmedError struct{ Detail string }

func (e *UnconfirmedError) Error() string { return e.Detail }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) poll() time.Duration {
	if s.PollEvery > 0 {
		return s.PollEvery
	}
	return 50 * time.Millisecond
}

// sessionView holds a possibly-nil Session so callers can tell "not read" from "read".
type sessionView struct{ s *kagentapi.Session }

// pgxTx runs fn in a transaction.
func pgxTx(ctx context.Context, s *Service, fn func(q *dbq.Queries) error) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error { return fn(dbq.New(tx)) })
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
