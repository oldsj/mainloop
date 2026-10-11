// Package hitl is the human-in-the-loop slice: it watches kagent for tasks that wait for the
// owner (tool approvals and questions), shows them as inbox cards, and carries the owner's answer
// back to the task it came from.
//
// Three parts share the hitl_* tables (see migration 00070):
//
//   - The observer is a sweep. It lists each bound session's tasks (read-only), adopts a task that
//     waits with a valid ask as a hitl_requests row plus an attention_items row, and keeps the
//     row honest: confirmed while kagent still shows the same ask, stale when kagent has been
//     silent for a while, unavailable when the task moved on, answered when the owner's response
//     shows up in the task. A failed pass writes nothing, so an outage loses no checkpoint.
//   - The responder records the owner's answer as an intent row (hitl_responses) with its
//     immutable receipt. A repeat of the same action id reads back the same receipt.
//   - The dispatcher is a reconciler loop over those rows. It checks the task first, marks the row
//     sending and commits, then sends once with no transaction open. Whatever it cannot confirm
//     it records as uncertain and settles by looking the message id up in the task; it never sends
//     a message twice.
package hitl

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/kagent"
)

// ResponseChannel wakes the dispatcher when a response is recorded.
const ResponseChannel = "mainloop_hitl_responses"

// Errors the API maps to the contract's statuses.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid")
	ErrDisabled = errors.New("owner HITL writes are disabled")
)

// ErrSendUnsupported is returned by a Kagent that cannot send a HITL response. The response stays
// recorded and is tried again later; nothing was sent.
var ErrSendUnsupported = errors.New("hitl: this kagent client cannot send HITL responses")

// Send is one native response to a waiting task.
type Send struct {
	// MessageID is the A2A message id. It is stable across retries and is the only handle for
	// finding the message in the task again.
	MessageID string
	ContextID string
	TaskID    string
	// Native is the response as the HITL extension defines it.
	Native map[string]any
}

// Kagent is the part of kagent this package uses. The reads are the sessions package's own; the
// send exists only here, because the shared client sends text only.
type Kagent interface {
	ListTasksPage(ctx context.Context, agent kagent.AgentRef, contextID, token string, limit int) ([]*a2a.Task, string, error)
	GetTask(ctx context.Context, agent kagent.AgentRef, taskID string) (*a2a.Task, error)
	FindTaskForMessage(ctx context.Context, agent kagent.AgentRef, contextID, messageID string) (*a2a.Task, bool, error)
	// SendHITL sends the response once. It returns when the stream ends, fails or ctx ends; the
	// caller reads the task to learn whether kagent took it. An error for which kagent.NotSent is
	// true, or ErrSendUnsupported, proves nothing was sent.
	SendHITL(ctx context.Context, agent kagent.AgentRef, s Send) error
}

// FromClient adapts the shared kagent client. Its SendHITL reports ErrSendUnsupported until
// kagent.SendParams can carry the extension metadata (see the G9a result note), so responses wait
// recorded in production until then; every read works.
func FromClient(c *kagent.Client) Kagent { return clientAdapter{c} }

type clientAdapter struct{ *kagent.Client }

func (clientAdapter) SendHITL(context.Context, kagent.AgentRef, Send) error {
	return ErrSendUnsupported
}

// Options tunes the observer and the dispatcher. Zero values take the defaults.
type Options struct {
	// ObserveEvery is the period of the observer sweep.
	ObserveEvery time.Duration
	// ScanEvery is how long a session rests after its task list was read to the end.
	ScanEvery time.Duration
	// RefreshEvery is how long a live request rests between confirmations.
	RefreshEvery time.Duration
	// StaleAfter is how long kagent may stay unreachable before a pending request shows as stale.
	StaleAfter time.Duration
	// MaxSessions, PagesPerScan, PageSize and RefreshBatch bound one pass.
	MaxSessions  int
	PagesPerScan int
	PageSize     int
	RefreshBatch int
	// ReadTimeout bounds one kagent read; SendTimeout bounds the send.
	ReadTimeout time.Duration
	SendTimeout time.Duration
	// UncertainHold is how long a response whose message no task shows stays blocking; after it
	// the response is failed and the request can be answered again.
	UncertainHold time.Duration
	// UncertainRecheck is the delay between lookups of an unresolved response.
	UncertainRecheck time.Duration
	// UnsupportedRetry is the delay before a response that could not be sent is tried again.
	UnsupportedRetry time.Duration
	// DiagnosticRepeat suppresses a repeated diagnostic for this long.
	DiagnosticRepeat time.Duration
	Concurrency      int
	Lease            time.Duration
}

func (o Options) withDefaults() Options {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	defInt := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&o.ObserveEvery, 5*time.Second)
	def(&o.ScanEvery, 30*time.Second)
	def(&o.RefreshEvery, 10*time.Second)
	def(&o.StaleAfter, 2*time.Minute)
	defInt(&o.MaxSessions, 10)
	defInt(&o.PagesPerScan, 3)
	defInt(&o.PageSize, 50)
	defInt(&o.RefreshBatch, 20)
	def(&o.ReadTimeout, 10*time.Second)
	def(&o.SendTimeout, 10*time.Second)
	def(&o.UncertainHold, 10*time.Minute)
	def(&o.UncertainRecheck, 15*time.Second)
	def(&o.UnsupportedRetry, time.Minute)
	def(&o.DiagnosticRepeat, 5*time.Minute)
	defInt(&o.Concurrency, 4)
	def(&o.Lease, 30*time.Second)
	return o
}

// Service owns the HITL tables. Methods are safe to call concurrently.
type Service struct {
	Pool    *pgxpool.Pool
	Kagent  Kagent
	OwnerID string
	// Agent is the kagent agent every Mainloop session runs on.
	Agent kagent.AgentRef
	// WritesEnabled gates answering. Reading and observing work either way.
	WritesEnabled bool
	Log           *slog.Logger
	Opts          Options
}

// NewService builds the service from the configuration.
func NewService(cfg *config.Config, pool *pgxpool.Pool, k Kagent, log *slog.Logger) *Service {
	return &Service{
		Pool: pool, Kagent: k, OwnerID: cfg.Identity.OwnerID, Log: log,
		Agent:         kagent.AgentRef{Namespace: cfg.Kagent.Namespace, Name: cfg.Kagent.MainAgent},
		WritesEnabled: bool(cfg.OwnerHITLWritesEnabled),
	}
}

func (s *Service) opts() Options { return s.Opts.withDefaults() }

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) agentOf(namespace, name string) kagent.AgentRef {
	if namespace == "" {
		return s.Agent
	}
	return kagent.AgentRef{Namespace: namespace, Name: name}
}

// isOutage reports whether err says kagent cannot be asked right now, so the rest of a pass would
// fail the same way. A definite answer about one task or session is not an outage.
func isOutage(err error) bool {
	var (
		unreachable *kagent.Unreachable
		unknown     *kagent.OutcomeUnknown
		cfg         *kagent.ConfigError
		proto       *kagent.ProtocolError
	)
	return errors.As(err, &unreachable) || errors.As(err, &unknown) || errors.As(err, &cfg) ||
		errors.As(err, &proto) || errors.Is(err, context.DeadlineExceeded)
}

// errDetail describes a failure for the owner without echoing kagent's text, which is untrusted.
func errDetail(err error) string {
	var (
		unreachable *kagent.Unreachable
		unknown     *kagent.OutcomeUnknown
		cfg         *kagent.ConfigError
		proto       *kagent.ProtocolError
		notFound    *kagent.TaskNotFound
		a2aErr      *kagent.A2AError
	)
	switch {
	case errors.As(err, &cfg):
		return "kagent refused the credentials"
	case errors.As(err, &unreachable):
		return "kagent is unreachable"
	case errors.Is(err, context.DeadlineExceeded):
		return "kagent did not answer in time"
	case errors.As(err, &unknown):
		return "kagent's answer was lost"
	case errors.As(err, &proto):
		return "kagent sent an unexpected reply"
	case errors.As(err, &notFound):
		return "kagent has no such task"
	case errors.As(err, &a2aErr):
		return "kagent refused the request"
	case errors.Is(err, ErrSendUnsupported):
		return "this build cannot send HITL responses yet"
	}
	return "the request failed"
}
