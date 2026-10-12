package health

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
)

// Kagent is the part of kagent the collector uses: reading one task with its artifacts.
type Kagent interface {
	GetTask(ctx context.Context, agent kagent.AgentRef, taskID string) (*a2a.Task, error)
}

// AgentResolver says which kagent Agent runs a session (the sessions package implements it). The
// reads must ask the Agent that owns the task.
type AgentResolver interface {
	ResolveAgent(workspaceNamespace, workspaceName, kind *string, role dbstate.BindingRole) (kagent.AgentRef, error)
}

// Defaults for the knobs a Service leaves zero.
const (
	defaultRecoverEvery   = 5 * time.Second
	defaultRetainEvery    = 10 * time.Minute
	defaultCollectTimeout = 10 * time.Second
	defaultBatch          = 50
	defaultMaxAttempts    = 8
	defaultFirstBackoff   = 15 * time.Second
	maxBackoff            = time.Hour
	// discoverBatch bounds the receipts one pass starts tracking.
	discoverBatch = 500
)

// Service collects, rolls up, retains and serves the owner's orchestration health.
type Service struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// OwnerID is the one owner whose health this instance holds.
	OwnerID string
	// Gateway names the kagent gateway the tasks were read from (config.Kagent.GatewayURL), so
	// observations of two gateways never share an identity.
	Gateway string
	// EventRetention and SummaryRetention are HEALTH_EVENT_RETENTION_DAYS and
	// HEALTH_SUMMARY_RETENTION_DAYS as durations.
	EventRetention   time.Duration
	SummaryRetention time.Duration
	// MaxChildrenPerParent and MaxChildrenGlobal are the task capacity limits the smoke ledger reports.
	MaxChildrenPerParent int
	MaxChildrenGlobal    int

	// Kagent is only needed by the recovery sweep; the routes never call kagent. Nil in a process
	// that does not reach it.
	Kagent Kagent

	// RecoverEvery and RetainEvery are the sweep periods. Zero takes the defaults.
	RecoverEvery time.Duration
	RetainEvery  time.Duration
	// CollectTimeout bounds one kagent read. Zero takes 10 seconds.
	CollectTimeout time.Duration
	// Batch is how many due receipts one pass reads. Zero takes 50.
	Batch int
	// MaxAttempts is how many reads a receipt gets before it is marked unavailable. Zero takes 8.
	MaxAttempts int
	// FirstBackoff is the delay after a receipt's first failed read; it doubles per attempt up to
	// an hour. Zero takes 15 seconds.
	FirstBackoff time.Duration

	// Now is the clock. Nil means time.Now; tests set it.
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func orDefault[T comparable](v, d T) T {
	var zero T
	if v == zero {
		return d
	}
	return v
}

// ErrNotFound is returned when a record is missing or is not the owner's.
var ErrNotFound = errors.New("not found")

// NewService builds a Service from configuration. k feeds the recovery sweep and may be nil in a
// process that does not reach kagent; the routes and the retention sweep work without it. The agent
// resolver the recovery sweep needs is passed to RecoverSweep, so the session service (which
// resolves agents) and this one never need each other at construction.
func NewService(cfg *config.Config, pool *pgxpool.Pool, k Kagent, log *slog.Logger) *Service {
	const day = 24 * time.Hour
	return &Service{
		Pool: pool, Kagent: k, Log: log, OwnerID: cfg.Identity.OwnerID, Gateway: cfg.Kagent.GatewayURL,
		EventRetention:       time.Duration(cfg.Health.EventRetentionDays) * day,
		SummaryRetention:     time.Duration(cfg.Health.SummaryRetentionDays) * day,
		MaxChildrenPerParent: cfg.Tasks.MaxChildrenPerParent, MaxChildrenGlobal: cfg.Tasks.MaxChildrenGlobal,
	}
}
