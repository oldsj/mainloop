// Package sessions is the main thread and its delivery ledger.
//
// A message to a native session is written once, as a user message and a delivery row in state
// recorded, in one transaction. A reconcile loop then sends it to kagent, follows the task and
// settles the row (see deliver.go). The ledger's states are explicit:
//
//	recorded   written; nothing has been sent
//	sending    the claimer is calling kagent; the outcome is not known yet
//	delivered  a task exists for the message; the turn is running
//	completed  the task finished and its reply is mirrored (final)
//	failed     not sent, rejected, or the task failed (final)
//	cancelled  the turn was stopped (final)
//	uncertain  the send may or may not have reached kagent; resolved by looking the message up
//
// A session has at most one open turn (recorded, sending or delivered), enforced by a partial
// unique index. A message is retried only when kagent says it was not accepted, with the same
// message id (the kagent client does that). Every other doubt becomes uncertain and is settled
// by GetTask or ListTasks, never by sending again.
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

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
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
)

// Kagent is the part of the kagent client the sessions use.
type Kagent interface {
	CreateSession(ctx context.Context, req *kagentapi.CreateSessionRequest) (*kagentapi.Session, error)
	GetSession(ctx context.Context, id string) (*kagentapi.Session, error)
	ResumeSession(ctx context.Context, id string) (*kagentapi.Session, error)
	SendMessage(ctx context.Context, agent kagent.AgentRef, p kagent.SendParams) iter.Seq2[a2a.Event, error]
	GetTask(ctx context.Context, agent kagent.AgentRef, taskID string) (*a2a.Task, error)
	FindTaskForMessage(ctx context.Context, agent kagent.AgentRef, contextID, messageID string) (*a2a.Task, bool, error)
	SubscribeToTask(ctx context.Context, agent kagent.AgentRef, taskID string) iter.Seq2[a2a.Event, error]
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
	// Lease is the delivery claim lease.
	Lease time.Duration
}

// OptionsFrom reads the settings.
func OptionsFrom(c config.Sessions) Options {
	return Options{
		UncertainHold:    c.UncertainHold.Duration(),
		UncertainRecheck: c.UncertainRecheck.Duration(),
		FollowPoll:       c.FollowPoll.Duration(),
		ParkedMax:        c.ParkedMax.Duration(),
		ReadyWait:        c.ReadyWait.Duration(),
		Concurrency:      c.DeliveryConcurrency,
		EnsureEvery:      c.EnsureEvery.Duration(),
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
	def(&o.Lease, 30*time.Second)
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
	// Agent is the kagent Agent that runs the main session.
	Agent kagent.AgentRef
	Log   *slog.Logger
	Opts  Options
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

// ensureMain returns the owner's main session, creating it (database rows only) if it is missing.
// The advisory lock makes concurrent first calls share one; the unique index backs it up.
func (s *Service) ensureMain(ctx context.Context, tx pgx.Tx) (dbq.GetMainBindingRow, error) {
	q := dbq.New(tx)
	row, err := q.GetMainBinding(ctx, s.OwnerID)
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return row, fmt.Errorf("get main session: %w", err)
	}
	if err := db.XactLock(ctx, tx, "mainloop:ensure-main:"+s.OwnerID); err != nil {
		return row, err
	}
	row, err = q.GetMainBinding(ctx, s.OwnerID)
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return row, fmt.Errorf("get main session: %w", err)
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
	if _, err := q.CreateAgentBinding(ctx, dbq.CreateAgentBindingParams{
		SessionID: id, Kind: "claude", Role: dbstate.RoleMain, MCPGrantKind: dbstate.GrantNone,
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
}

// Send records one user message for the main session and queues its delivery. It is idempotent
// per message id: sending the same id again returns the first result and changes nothing, so a
// client or caller that is unsure whether a request landed may repeat it. conversationID, when
// set, must be the main session's.
//
// It touches only the database. The delivery loop makes the kagent calls.
func (s *Service) Send(ctx context.Context, messageID, conversationID, text string) (Sent, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Sent{}, fmt.Errorf("%w: the message is empty", ErrInvalid)
	}
	if utf8.RuneCountInString(text) > MaxMessageRunes {
		return Sent{}, fmt.Errorf("%w: the message is too long", ErrInvalid)
	}
	if _, err := uuid.Parse(messageID); err != nil {
		return Sent{}, fmt.Errorf("%w: the message id is not a UUID", ErrInvalid)
	}
	var out Sent
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		main, err := s.ensureMain(ctx, tx)
		if err != nil {
			return err
		}
		if conversationID != "" && conversationID != main.SessionID {
			return ErrNotFound
		}
		out = Sent{ConversationID: main.SessionID, MessageID: messageID}
		// One send at a time per session, so the open-turn check below cannot race another send.
		if err := db.XactLock(ctx, tx, "mainloop:send:"+main.SessionID); err != nil {
			return err
		}
		if d, err := q.GetDelivery(ctx, messageID); err == nil {
			if d.SessionID != main.SessionID {
				return fmt.Errorf("%w: the message id belongs to another conversation", ErrInvalid)
			}
			return nil // already recorded
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get delivery: %w", err)
		}
		if _, err := q.GetBlockingDelivery(ctx, dbq.GetBlockingDeliveryParams{
			SessionID: main.SessionID, HoldSeconds: s.opts().UncertainHold.Seconds(),
		}); err == nil {
			return ErrBusy
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check open turn: %w", err)
		}
		if n, err := q.InsertSessionMessage(ctx, dbq.InsertSessionMessageParams{
			ID: messageID, SessionID: main.SessionID, Role: string(userRole), Content: text,
		}); err != nil {
			return fmt.Errorf("record message: %w", err)
		} else if n == 0 {
			// No delivery has this id, so the row is some other message: ids are global.
			return fmt.Errorf("%w: the message id is already used", ErrInvalid)
		}
		if _, err := q.InsertDelivery(ctx, dbq.InsertDeliveryParams{
			ID: messageID, SessionID: main.SessionID, Source: "user",
		}); err != nil {
			return fmt.Errorf("record delivery: %w", err)
		}
		if err := q.RefreshSessionStatus(ctx, main.SessionID); err != nil {
			return fmt.Errorf("refresh session status: %w", err)
		}
		// Delivered to listeners when the transaction commits.
		return q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: DeliveryChannel, Payload: messageID})
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "deliveries_one_open" {
		return Sent{}, ErrBusy
	}
	if err != nil {
		return Sent{}, err
	}
	return out, nil
}

// NewService builds the service from settings. k may be nil in a process that does not talk to
// kagent.
func NewService(cfg *config.Config, pool *pgxpool.Pool, k Kagent, log *slog.Logger) *Service {
	return &Service{
		Pool: pool, Kagent: k, OwnerID: cfg.Identity.OwnerID, Log: log,
		Agent: kagent.AgentRef{Namespace: cfg.Kagent.Namespace, Name: cfg.Kagent.MainAgent},
		Opts:  OptionsFrom(cfg.Sessions),
	}
}
