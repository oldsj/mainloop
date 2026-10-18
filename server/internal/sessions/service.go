// Package sessions is the main thread and its delivery ledger.
//
// A message to a native session is written once, as a user message and a delivery row in state
// recorded (or queued, while another turn is open), in one transaction. A reconcile loop then sends
// a recorded message to kagent, follows the task and settles the row (see deliver.go). The ledger's
// states are explicit:
//
//	queued     written while another turn was open; waiting its turn, not yet admitted
//	recorded   admitted; nothing has been sent
//	sending    the claimer is calling kagent; the outcome is not known yet
//	delivered  a task exists for the message; the turn is running
//	completed  the task finished and its reply is mirrored (final)
//	failed     not sent, rejected, or the task failed (final)
//	cancelled  the turn was stopped (final)
//	uncertain  the send may or may not have reached kagent; resolved by looking the message up
//
// A session has at most one open turn (recorded, sending or delivered), enforced by a partial
// unique index; queued is deliberately excluded from it, so many messages may wait behind the open
// one. Whenever a turn ends (settles in deliver.go, or is stopped in control.go), the oldest queued
// message of that session, if any, is admitted (moved to recorded) in the same transaction: the
// index has nothing left of the session's to conflict with. A message is retried only when kagent
// says it was not accepted, with the same message id (the kagent client does that). Every other
// doubt becomes uncertain and is settled by GetTask or ListTasks, never by sending again.
package sessions

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/progress"
	"github.com/oldsj/mainloop/server/internal/providers"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// DeliveryChannel is the notification channel that wakes the delivery loop.
const DeliveryChannel = "mainloop_deliveries"

// MaxMessageRunes bounds one chat message.
const MaxMessageRunes = 100_000

// Errors the API maps to the contract's statuses.
var (
	// ErrNotFound is a conversation that is not the owner's.
	ErrNotFound = errors.New("sessions: conversation not found")
	// ErrBusy is a send while the session has an open turn.
	ErrBusy = errors.New("sessions: a turn is already in progress")
	// ErrInvalid is a message that cannot be sent.
	ErrInvalid = errors.New("sessions: invalid message")
	// ErrConflict is a request the session's state does not allow: no agent binding, ended or
	// archived, or not stoppable. The wrapping text is shown to the caller.
	ErrConflict = errors.New("sessions: conflict")
)

// Stable codes of a refused main-thread provider switch (see CodedHTTPError in the contract).
const (
	codeBusy             = "busy"
	codePending          = "pending"
	codeUnconfirmed      = "unconfirmed"
	codeUnresolved       = "unresolved"
	codeAgentUnavailable = "agent_unavailable"
)

// codedConflict is an ErrConflict that also carries a stable machine code.
type codedConflict struct {
	code, msg string
}

func (e *codedConflict) Error() string { return ErrConflict.Error() + ": " + e.msg }
func (e *codedConflict) Unwrap() error { return ErrConflict }

// conflictCode is conflict with a stable code.
func conflictCode(code, format string, args ...any) error {
	return &codedConflict{code: code, msg: fmt.Sprintf(format, args...)}
}

// conflict is an ErrConflict with the reason shown to the caller.
func conflict(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, args...))
}

var errTaskSessionStopped = conflict("This task attempt has stopped; start a new one.")

// checkTaskSendFence reads the durable stop intent, including old attempts. Session activity can
// become completed without reopening the attempt's authority to send.
func checkTaskSendFence(ctx context.Context, q *dbq.Queries, sessionID string) error {
	state, err := q.LockSessionTaskAttempt(ctx, &sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // standalone and adopted scheduled sessions have no task attempt
	}
	if err != nil {
		return fmt.Errorf("check task session send fence: %w", err)
	}
	if state != "none" {
		return errTaskSessionStopped
	}
	return nil
}

// reason is the text of an error that wraps sentinel, without the sentinel.
func reason(err, sentinel error) string {
	return strings.TrimPrefix(err.Error(), sentinel.Error()+": ")
}

// Kagent is the part of the kagent client the sessions use.
type Kagent interface {
	CreateSession(ctx context.Context, req *kagentapi.CreateSessionRequest) (*kagentapi.Session, error)
	GetSession(ctx context.Context, id string) (*kagentapi.Session, error)
	// ObserveSession is GetSession for display: a Session without a confirmed context is returned.
	ObserveSession(ctx context.Context, id string) (*kagentapi.Session, error)
	DeleteSession(ctx context.Context, id string) (*kagentapi.Session, error)
	ResumeSession(ctx context.Context, id string) (*kagentapi.Session, error)
	SuspendSession(ctx context.Context, id string) (*kagentapi.Session, error)
	GetAgent(ctx context.Context, ref kagent.AgentRef) (*kagentapi.Agent, error)
	SendMessage(ctx context.Context, agent kagent.AgentRef, p kagent.SendParams) iter.Seq2[a2a.Event, error]
	GetTask(ctx context.Context, agent kagent.AgentRef, taskID string) (*a2a.Task, error)
	ListTasks(ctx context.Context, agent kagent.AgentRef, contextID string) ([]*a2a.Task, error)
	FindTaskForMessage(ctx context.Context, agent kagent.AgentRef, contextID, messageID string) (*a2a.Task, bool, error)
	SubscribeToTask(ctx context.Context, agent kagent.AgentRef, taskID string) iter.Seq2[a2a.Event, error]
	CancelTask(ctx context.Context, agent kagent.AgentRef, taskID string) (*a2a.Task, error)
}

// HealthSink stores a finished delivery's health observations in the transaction that ends the
// delivery. *health.Service satisfies it.
type HealthSink interface {
	IngestDelivery(
		ctx context.Context, tx pgx.Tx, sessionID, deliveryID, runtimeSessionID string, createdAt time.Time, task *a2a.Task,
	) error
}

// Waker makes a workspace's runtime ready for a message: it waits for an open suspend to finish and
// resumes the workspace if it is suspended. *workspaces.Service satisfies it. A nil Waker leaves a
// suspended Session to the delivery loop's own resume.
type Waker interface {
	Wake(ctx context.Context, workspaceID string) error
}

// Options tunes the ledger. Zero values take the defaults.
type Options struct {
	// UncertainHold keeps an unresolved delivery blocking new sends, and after it a message that
	// no task shows is failed.
	UncertainHold time.Duration
	// UncertainRecheck is the delay between lookups of an unresolved delivery.
	UncertainRecheck time.Duration
	// FollowPoll is the delay before a running task is read again.
	FollowPoll time.Duration
	// ParkedMax caps the growing delay between reads of a parked task.
	ParkedMax time.Duration
	// ReadyWait bounds the wait for a kagent Session to become ready.
	ReadyWait time.Duration
	// Concurrency is how many deliveries are handled at once.
	Concurrency int
	// EnsureEvery is the period of the main-session sweep.
	EnsureEvery time.Duration
	// ResolveEvery is the period of the sweep that settles unresolved Session creates.
	ResolveEvery time.Duration
	// Lease is the delivery claim lease.
	Lease time.Duration
	// CarryOverMessages is how many recent completed messages the first message to a new main
	// Session carries. The zero value carries none; OptionsFrom sets the configured default.
	CarryOverMessages int
	// StallAfter is how long a delivered turn may show no task event before it reads as stalled.
	StallAfter time.Duration
	// StallToolAfter replaces StallAfter while the last event left a tool call running.
	StallToolAfter time.Duration
	// StallEvery is the period of the stall sweep.
	StallEvery time.Duration
	// ActivityWriteEvery is the shortest time between two writes of a turn's activity time; a change
	// of what the turn is doing, or the end of a stall, is written at once.
	ActivityWriteEvery time.Duration
}

// OptionsFrom reads the settings.
func OptionsFrom(c config.Sessions) Options {
	return Options{
		CarryOverMessages: c.CarryOverMessages,
		UncertainHold:     c.UncertainHold.Duration(),
		UncertainRecheck:  c.UncertainRecheck.Duration(),
		FollowPoll:        c.FollowPoll.Duration(),
		ParkedMax:         c.ParkedMax.Duration(),
		ReadyWait:         c.ReadyWait.Duration(),
		Concurrency:       c.DeliveryConcurrency,
		EnsureEvery:       c.EnsureEvery.Duration(),
		StallAfter:        c.StallAfter.Duration(),
		StallToolAfter:    c.StallToolAfter.Duration(),
	}
}

func (o Options) withDefaults() Options {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.UncertainHold, 30*time.Minute)
	def(&o.UncertainRecheck, 30*time.Second)
	def(&o.FollowPoll, 2*time.Second)
	def(&o.ParkedMax, 5*time.Minute)
	def(&o.ReadyWait, time.Minute)
	def(&o.EnsureEvery, time.Minute)
	def(&o.ResolveEvery, 5*time.Minute)
	def(&o.Lease, 30*time.Second)
	def(&o.StallAfter, 25*time.Minute)
	def(&o.StallToolAfter, time.Hour)
	if o.StallToolAfter < o.StallAfter {
		o.StallToolAfter = o.StallAfter
	}
	def(&o.StallEvery, 30*time.Second)
	def(&o.ActivityWriteEvery, 30*time.Second)
	if o.Concurrency <= 0 {
		o.Concurrency = 16
	}
	return o
}

// Service owns the main thread and the delivery ledger.
type Service struct {
	Pool *pgxpool.Pool
	// Kagent may be nil in a process that only serves reads: sends then stay recorded.
	Kagent  Kagent
	OwnerID string
	// Agent is the kagent Agent that runs a main session that has no Agent of its own: the Claude
	// main Agent. A switch pins the Agent of the target provider on the binding.
	Agent kagent.AgentRef
	// MainProvider is the provider a main thread starts with, claude or codex. An existing main
	// keeps its own selection.
	MainProvider string
	// MainCodexAgent is the Agent a Codex main thread runs on, used when there is no registry.
	MainCodexAgent kagent.AgentRef
	// AfterIntent is a test seam: it runs after a main switch intent commits and before the create;
	// an error it returns ends the request there, as a crash would.
	AfterIntent func(ctx context.Context) error
	// AfterSuspend is a test seam: it runs after a switch suspended the old native Session and before
	// the intent commits; an error it returns ends the request there, as a crash would.
	AfterSuspend func(ctx context.Context) error
	// Providers resolves a session's Agent from its binding, and a new session's provider.
	Providers *providers.Registry
	// Waker, when set, readies a workspace session's runtime before each send.
	Waker Waker
	// Credentials publishes the MCP credential of a binding that needs one before its kagent Session
	// is created, and revokes it when the session ends. Nil creates sessions without MCP tools.
	Credentials *agentauth.Credentials
	// MCPOrigin is where those credentials are valid: the MCP Service's in-cluster URL.
	MCPOrigin string
	// Health, when set, stores a finished turn's health observations in the transaction that ends it.
	// The health recovery sweep collects whatever this misses, so nil only costs a later read.
	Health HealthSink
	// Progress shows a running turn's reply and tool calls live. Nil shows none.
	Progress *progress.Tracker
	Log      *slog.Logger
	Opts     Options
	// Now is the clock of the stall watch (activity times and the stall sweep). Nil is time.Now.
	Now func() time.Time
}

func (s *Service) opts() Options { return s.Opts.withDefaults() }

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Main is the owner's main session.
type Main struct {
	SessionID       string
	Kind            string
	Role            dbstate.BindingRole
	ParentSessionID *string
	TopicID         *string
	KagentSessionID *string
	Model           *string
	Turns           int
}

func mainFrom(r dbq.GetMainBindingRow) Main {
	return Main{
		SessionID: r.SessionID, Kind: r.Kind, Role: r.Role, ParentSessionID: r.ParentSessionID,
		TopicID: r.TopicID, KagentSessionID: r.KagentSessionID, Model: r.Model, Turns: int(r.Turns),
	}
}

// mainActive reports whether the main binding still has coordination authority: it is a
// coordination grant on a session that has not ended or been archived. A main that lost it is
// replaced.
func mainActive(r dbq.GetMainBindingRow) bool {
	return r.MCPGrantKind == dbstate.GrantCoordination && !endedStatus(r.SessionStatus) &&
		r.SessionStatus != dbstate.SessionCompleted
}

// ensureMain returns the owner's main session, creating it (database rows only) if it is missing.
// A main whose binding no longer has active coordination authority is replaced: its owner slot is
// released and its credential revoked in the transaction that creates the replacement. The
// advisory lock makes concurrent first calls share one; the unique index backs it up.
func (s *Service) ensureMain(ctx context.Context, tx pgx.Tx) (dbq.GetMainBindingRow, error) {
	q := dbq.New(tx)
	row, err := q.GetMainBinding(ctx, s.OwnerID)
	if err == nil && mainActive(row) {
		return row, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return row, fmt.Errorf("get main session: %w", err)
	}
	if err := db.XactLock(ctx, tx, "mainloop:ensure-main:"+s.OwnerID); err != nil {
		return row, err
	}
	row, err = q.GetMainBinding(ctx, s.OwnerID)
	if err == nil && mainActive(row) {
		return row, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return row, fmt.Errorf("get main session: %w", err)
	}
	if err == nil {
		// The old binding keeps its row, history and Session. It stops owning the slot and
		// authenticating in the same transaction that creates its replacement.
		if _, err := agentauth.RevokeInTx(ctx, q, row.SessionID); err != nil {
			return row, fmt.Errorf("revoke the released main's credential: %w", err)
		}
		if _, err := q.ReleaseMainBinding(ctx, row.SessionID); err != nil {
			return row, fmt.Errorf("release the main session: %w", err)
		}
	}
	id := uuid.NewString()
	if _, err := q.CreateSession(ctx, dbq.CreateSessionParams{
		ID: id, UserID: s.OwnerID, Title: "Main thread", Description: "Native main thread",
		BaseBranch: "main",
	}); err != nil {
		return row, fmt.Errorf("create main session: %w", err)
	}
	if _, err := q.SetSessionStatus(ctx, dbq.SetSessionStatusParams{ID: id, Status: dbstate.SessionIdle}); err != nil {
		return row, fmt.Errorf("create main session: %w", err)
	}
	if _, err := q.CreateMainBinding(ctx, dbq.CreateMainBindingParams{
		SessionID: id, Kind: s.mainProvider(),
	}); err != nil {
		return row, fmt.Errorf("create main binding: %w", err)
	}
	return q.GetMainBinding(ctx, s.OwnerID)
}

// EnsureMain creates the main session if it does not exist yet and returns it. The sweep calls
// it; GET /main-thread never does.
func (s *Service) EnsureMain(ctx context.Context) (Main, error) {
	var m Main
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		row, err := s.ensureMain(ctx, tx)
		m = mainFrom(row)
		return err
	})
	return m, err
}

// Sent is what a send returns.
type Sent struct {
	ConversationID string
	MessageID      string
	// Queued is whether the message was recorded as queued, behind an open turn, instead of
	// admitted at once.
	Queued bool
}

// checkText trims and validates a message and its id.
func checkText(messageID, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("%w: the message is empty", ErrInvalid)
	}
	if utf8.RuneCountInString(text) > MaxMessageRunes {
		return "", fmt.Errorf("%w: the message is too long", ErrInvalid)
	}
	if _, err := uuid.Parse(messageID); err != nil {
		return "", fmt.Errorf("%w: the message id is not a UUID", ErrInvalid)
	}
	return text, nil
}

// Source is who wrote a message. The owner's own messages are SourceUser; SourceMainloop marks the ones
// Mainloop writes on the owner's behalf: a delegated brief, or a task report sent to a supervisor. The
// values are the contract's Message.source and what deliveries.source allows.
type Source string

const (
	SourceUser     Source = "user"
	SourceMainloop Source = "mainloop"
)

// record writes a message from source and its delivery for a session, in tx, and reports whether it
// was queued behind an open turn rather than admitted at once. It is idempotent per message id: an
// id that is already recorded for the session changes nothing and reports the state it is actually
// in. The caller has checked that the session exists and may take a message.
func (s *Service) record(ctx context.Context, tx pgx.Tx, source Source, sessionID, messageID, text string) (bool, error) {
	q := dbq.New(tx)
	// One send at a time per session, so the open-turn check below cannot race another send.
	if err := db.XactLock(ctx, tx, "mainloop:send:"+sessionID); err != nil {
		return false, err
	}
	if err := checkTaskSendFence(ctx, q, sessionID); err != nil {
		return false, err
	}
	if err := checkMainReady(ctx, q, sessionID); err != nil {
		return false, err
	}
	if d, err := q.GetDelivery(ctx, messageID); err == nil {
		if d.SessionID != sessionID {
			return false, fmt.Errorf("%w: the message id belongs to another conversation", ErrInvalid)
		}
		return d.State == dbstate.DeliveryQueued, nil // already recorded
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("get delivery: %w", err)
	}
	hold := s.opts().UncertainHold.Seconds()
	busy := false
	if _, err := q.GetBlockingDelivery(ctx, dbq.GetBlockingDeliveryParams{
		SessionID: sessionID, HoldSeconds: hold,
	}); err == nil {
		busy = true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("check open turn: %w", err)
	}
	hasQueued, err := q.HasQueuedDelivery(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("check queued backlog: %w", err)
	}
	if !busy && hasQueued {
		// The turn that blocked the backlog has ended or gone past its hold, but nothing released
		// it yet: release the oldest queued message now, under the same lock, before this one takes
		// a place behind it.
		promoted, err := promoteNextQueued(ctx, q, sessionID, hold)
		if err != nil {
			return false, fmt.Errorf("promote queued delivery: %w", err)
		}
		if promoted != "" {
			if err := q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: DeliveryChannel, Payload: promoted}); err != nil {
				return false, fmt.Errorf("notify promoted delivery: %w", err)
			}
		}
	}
	// A message behind an existing backlog queues FIFO, even once the turn that first blocked it is
	// no longer open: whatever was promoted above now holds the slot ahead of this one.
	busy = busy || hasQueued
	if n, err := q.InsertSessionMessage(ctx, dbq.InsertSessionMessageParams{
		ID: messageID, SessionID: sessionID, Role: string(userRole), Content: text,
	}); err != nil {
		return false, fmt.Errorf("record message: %w", err)
	} else if n == 0 {
		// No delivery has this id, so the row is some other message: ids are global.
		return false, fmt.Errorf("%w: the message id is already used", ErrInvalid)
	}
	if busy {
		// Durably queued, behind the open turn: deliveries_one_open excludes this state, so the
		// insert cannot conflict with it. The reconcile loop never claims a queued row; whoever ends
		// the open turn admits this one, in the order it arrived.
		if _, err := q.InsertQueuedDelivery(ctx, dbq.InsertQueuedDeliveryParams{
			ID: messageID, SessionID: sessionID, Source: string(source),
		}); err != nil {
			return false, fmt.Errorf("record queued delivery: %w", err)
		}
	} else if _, err := q.InsertDelivery(ctx, dbq.InsertDeliveryParams{
		ID: messageID, SessionID: sessionID, Source: string(source),
	}); err != nil {
		return false, fmt.Errorf("record delivery: %w", err)
	}
	// The owner's next message releases the hold a stop left on the queue.
	if err := q.SetBindingQueueHeld(ctx, dbq.SetBindingQueueHeldParams{SessionID: sessionID, QueueHeld: false}); err != nil {
		return false, fmt.Errorf("release queue hold: %w", err)
	}
	if err := q.RefreshSessionStatus(ctx, sessionID); err != nil {
		return false, fmt.Errorf("refresh session status: %w", err)
	}
	if busy {
		return true, nil
	}
	// Delivered to listeners when the transaction commits.
	return false, q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: DeliveryChannel, Payload: messageID})
}

// promoteNextQueued admits the oldest message still queued for the session, if any, once nothing
// blocks it any more: no open turn, and no uncertain one still inside holdSeconds. It returns the
// promoted delivery's id, or "" when nothing was queued or the slot is still held; the caller
// notifies DeliveryChannel so the reconcile loop picks it up at once. The caller holds
// mainloop:send:<session> for the duration, serializing this with every other admission and
// promotion of the session.
func promoteNextQueued(ctx context.Context, q *dbq.Queries, sessionID string, holdSeconds float64) (string, error) {
	id, err := q.PromoteNextQueuedDelivery(ctx, dbq.PromoteNextQueuedDeliveryParams{
		SessionID: sessionID, HoldSeconds: holdSeconds,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// busyOrErr maps the database's one-open-turn index to ErrBusy.
func busyOrErr(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "deliveries_one_open" {
		return ErrBusy
	}
	return err
}

// Send records one user message for the main session and queues its delivery. It is idempotent
// per message id: sending the same id again returns the first result and changes nothing, so a
// client or caller that is unsure whether a request landed may repeat it. conversationID, when
// set, must be the main session's.
//
// It touches only the database. The delivery loop makes the kagent calls.
func (s *Service) Send(ctx context.Context, messageID, conversationID, text string) (Sent, error) {
	return s.SendAs(ctx, SourceUser, messageID, conversationID, text)
}

// SendAs is Send for a message from source (SourceUser or SourceMainloop).
func (s *Service) SendAs(ctx context.Context, source Source, messageID, conversationID, text string) (Sent, error) {
	text, err := checkText(messageID, text)
	if err != nil {
		return Sent{}, err
	}
	var out Sent
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		main, err := s.ensureMain(ctx, tx)
		if err != nil {
			return err
		}
		if conversationID != "" && conversationID != main.SessionID {
			return ErrNotFound
		}
		out = Sent{ConversationID: main.SessionID, MessageID: messageID}
		queued, err := s.record(ctx, tx, source, main.SessionID, messageID, text)
		out.Queued = queued
		return err
	})
	if err = busyOrErr(err); err != nil {
		return Sent{}, err
	}
	return out, nil
}

// endedStatus is a status after which a session takes no more messages.
func endedStatus(st dbstate.SessionStatus) bool {
	return st == dbstate.SessionCancelled || st == dbstate.SessionFailed
}

// SendTo records one user message for a session of the owner and queues its delivery, like Send.
// The session must have a native binding, must not have ended or been archived, and must have no
// open turn or stop request on its task attempt.
func (s *Service) SendTo(ctx context.Context, sessionID, messageID, text string) (Sent, error) {
	return s.SendToAs(ctx, SourceUser, sessionID, messageID, text)
}

// SendToAs is SendTo for a message from source (SourceUser or SourceMainloop).
func (s *Service) SendToAs(ctx context.Context, source Source, sessionID, messageID, text string) (Sent, error) {
	text, err := checkText(messageID, text)
	if err != nil {
		return Sent{}, err
	}
	var queued bool
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		// The lock cancel also takes, before the status is read: no message is recorded after a
		// cancel commits. Stop and archive take no lock; the open-turn index and the status check
		// already keep them consistent with a send.
		if err := db.XactLock(ctx, tx, "mainloop:send:"+sessionID); err != nil {
			return err
		}
		q := dbq.New(tx)
		b, err := q.GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: s.OwnerID})
		if errors.Is(err, pgx.ErrNoRows) {
			// Either not the owner's, or a session without a native binding.
			if sess, serr := q.GetSession(ctx, sessionID); serr == nil && sess.UserID == s.OwnerID {
				return conflict("Session has no native agent binding")
			}
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get binding: %w", err)
		}
		if endedStatus(b.SessionStatus) {
			return conflict("This session is %s; start a new one.", b.SessionStatus)
		}
		if b.ArchivedAt != nil {
			return conflict("This session is archived; start a new one.")
		}
		if b.WorkspaceAgentName != nil && b.KagentSessionID == nil {
			return conflict("The workspace is not ready yet; try again when it is.")
		}
		var rerr error
		queued, rerr = s.record(ctx, tx, source, sessionID, messageID, text)
		return rerr
	})
	if err = busyOrErr(err); err != nil {
		return Sent{}, err
	}
	return Sent{ConversationID: sessionID, MessageID: messageID, Queued: queued}, nil
}

// NewService builds the service from settings. k may be nil in a test that talks to no kagent. reg
// resolves providers; creds publishes the MCP credential of a binding before its Session is created;
// health stores a finished turn's observations in the transaction that ends it; waker readies a
// workspace session's runtime before each send. A test with none of these passes nil.
func NewService(cfg *config.Config, pool *pgxpool.Pool, k Kagent, reg *providers.Registry, creds *agentauth.Credentials,
	health HealthSink, waker Waker, prog *progress.Tracker, log *slog.Logger) *Service {
	return &Service{
		Pool: pool, Kagent: k, OwnerID: cfg.Identity.OwnerID, Log: log,
		Providers: reg, Credentials: creds, Health: health, Waker: waker, Progress: prog,
		Agent:          kagent.AgentRef{Namespace: cfg.Kagent.Namespace, Name: cfg.Kagent.MainAgent},
		MainProvider:   cfg.Main.Provider,
		MainCodexAgent: kagent.AgentRef{Namespace: cfg.Kagent.Namespace, Name: cfg.Kagent.MainCodexAgent},
		Opts:           OptionsFrom(cfg.Sessions),
		MCPOrigin:      cfg.MCPOrigin(),
	}
}
