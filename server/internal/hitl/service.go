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

	"connectrpc.com/connect"
	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
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
// send carries the HITL extension metadata.
type Kagent interface {
	// ListWaitingTasksPage lists the tasks of a conversation that wait for input, with at most their
	// latest history message.
	ListWaitingTasksPage(ctx context.Context, agent kagent.AgentRef, contextID, token string, limit int) ([]*a2a.Task, string, error)
	GetTask(ctx context.Context, agent kagent.AgentRef, taskID string) (*a2a.Task, error)
	FindTaskForMessage(ctx context.Context, agent kagent.AgentRef, contextID, messageID string) (*a2a.Task, bool, error)
	// SendHITL sends the response once. It returns when the stream ends, fails or ctx ends; the
	// caller reads the task to learn whether kagent took it. An error for which kagent.NotSent is
	// true proves nothing was sent.
	SendHITL(ctx context.Context, agent kagent.AgentRef, s Send) error
}

// AgentResolver says which kagent Agent runs a session. kagent scopes a conversation to the Agent
// that owns it and refuses any other, so the observer and the dispatcher must ask the right one.
// The sessions package implements it.
type AgentResolver interface {
	ResolveAgent(workspaceNamespace, workspaceName, kind *string, role dbstate.BindingRole) (kagent.AgentRef, error)
}

// SessionReadier makes a session's kagent Session ready to take a send: it wakes a workspace and
// resumes a suspended Session, and sends nothing. kagent refuses any send to a Session that is not
// ready. The sessions package implements it, by the rule deliveries use.
type SessionReadier interface {
	ReadySession(ctx context.Context, sessionID string, workspaceID *string, kagentSessionID string) error
}

// TurnWaker wakes the delivery that follows a task, once kagent has accepted the owner's answer to it.
// The sessions package implements it.
type TurnWaker interface {
	WakeTurn(ctx context.Context, kagentSessionID, taskID string) error
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
	// the response is failed and the request can be answered again. It must stay above kagent's
	// 2-minute dispatch reservation: a send that reached kagent but was never confirmed cannot be
	// saved after the reservation expires, so a response failed after the hold can be answered again
	// without the agent ever seeing two answers. Lowering it under 2 minutes breaks that.
	UncertainHold time.Duration
	// UncertainRecheck is the delay before the first lookup of an unresolved response; it doubles
	// after each lookup up to UncertainCeiling.
	UncertainRecheck time.Duration
	UncertainCeiling time.Duration
	// ScanBackoff is the delay before a session that failed to scan is tried again; it doubles
	// with each failure in a row up to ScanBackoffCeiling.
	ScanBackoff        time.Duration
	ScanBackoffCeiling time.Duration
	// StaleRetry is the delay before a response is tried again after it provably was not sent, or while its request is stale.
	StaleRetry time.Duration
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
	def(&o.UncertainCeiling, time.Hour)
	def(&o.ScanBackoff, 30*time.Second)
	def(&o.ScanBackoffCeiling, 10*time.Minute)
	def(&o.StaleRetry, time.Minute)
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
	// Agents resolves the Agent that runs each session, by the sessions package's rule. Without it
	// every session is taken to run on Agent.
	Agents AgentResolver
	// Ready makes a session's kagent Session ready before an answer is sent. Without it the answer is
	// sent as it is.
	Ready SessionReadier
	// Turns is woken when an answer is accepted, so the delivery that follows the task reads it at once.
	// Without it the delivery finds the resumed task on its own parked backoff.
	Turns TurnWaker
	// Agent is the main Agent: the one sessions run on when Agents is nil.
	Agent kagent.AgentRef
	// WritesEnabled gates answering. Reading and observing work either way.
	WritesEnabled bool
	Log           *slog.Logger
	Opts          Options
	// MergeConfigs binds native tool names to the runtime's named template and provider.
	MergeConfigs []config.MergeConfiguration
	MergeRuntime MergeRuntime
}

// NewService builds the service from the configuration. agents resolves the Agent that runs each
// session, ready makes a session ready before an answer is sent and turns wakes the delivery of an
// answered task; all three are the session service in the server, and nil in a test that has no
// sessions (every session is then taken to run on the main Agent, the answer is sent as it is and no
// delivery is woken).
func NewService(cfg *config.Config, pool *pgxpool.Pool, k Kagent, agents AgentResolver, ready SessionReadier, turns TurnWaker, log *slog.Logger) *Service {
	runtime, _ := k.(MergeRuntime)
	return &Service{
		Pool: pool, Kagent: k, OwnerID: cfg.Identity.OwnerID, Log: log, Agents: agents, Ready: ready, Turns: turns,
		Agent:         kagent.AgentRef{Namespace: cfg.Kagent.Namespace, Name: cfg.Kagent.MainAgent},
		WritesEnabled: bool(cfg.OwnerHITLWritesEnabled),
		MergeConfigs:  cfg.MergeConfigurations, MergeRuntime: runtime,
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

// isOutage reports whether err says kagent could not answer: it was unreachable, refused the
// credentials, lost the reply or sent nonsense. It does not say whose fault that is: the same
// errors come from one bad session and from a dead gateway.
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

// isKagentError reports whether err came from kagent (or its absence) rather than from our own
// database, so it says something about the session that was asked.
func isKagentError(err error) bool {
	var (
		notFound *kagent.TaskNotFound
		a2aErr   *kagent.A2AError
		session  *kagent.SessionError
	)
	return isOutage(err) || errors.As(err, &notFound) || errors.As(err, &a2aErr) || errors.As(err, &session)
}

// backoff is base doubled n times, held at ceiling.
func backoff(base, ceiling time.Duration, n int) time.Duration {
	d := base
	for range n {
		if d >= ceiling {
			break
		}
		d *= 2
	}
	return min(d, ceiling)
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
		sessErr     *kagent.SessionError
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
	case errors.As(err, &sessErr):
		// By code only: the error's Detail is kagent's own text.
		switch sessErr.Code {
		case connect.CodeNotFound:
			return "kagent has no such session"
		case connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeAlreadyExists:
			return "kagent refused the session request"
		}
	}
	return detailUnclassified
}

// detailUnclassified is errDetail's answer for an error it has no description for.
const detailUnclassified = "the request failed"
