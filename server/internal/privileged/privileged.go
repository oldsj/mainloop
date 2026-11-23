// Package privileged runs credentialed actions an agent requests and the owner approves from the
// phone (docs/specs/privileged-actions.md). An agent requests a catalog action; Mainloop
// canonicalizes its arguments, writes the intent row, runs a read-only preflight with no
// transaction open, freezes what it found under a digest and opens one approval card. The owner's
// Approve must carry that digest. The executor then runs exactly that action once, observing the
// remote before any retry, and the outcome goes back to the requesting session as one message.
//
// Slice 1 has two types, github.pr_merge and github.pr_close, run through Mainloop's own GitHub
// App. Nothing bypasses a branch rule.
package privileged

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/events"
)

// States of an action. The CHECK on privileged_actions.state names exactly these.
const (
	StateRequested       = "requested"
	StatePendingApproval = "pending_approval"
	StatePreflightFailed = "preflight_failed"
	StateApproved        = "approved"
	StateExecuting       = "executing"
	StateSucceeded       = "succeeded"
	StateFailed          = "failed"
	StateStale           = "stale"
	StateDenied          = "denied"
	StateExpired         = "expired"
	StateCancelled       = "cancelled"
	StateUncertain       = "uncertain"
)

// States lists every state, for the schema check test.
var States = []string{
	StateRequested, StatePendingApproval, StatePreflightFailed, StateApproved, StateExecuting,
	StateSucceeded, StateFailed, StateStale, StateDenied, StateExpired, StateCancelled, StateUncertain,
}

// Delivery states of an action's outcome message. The CHECK on privileged_actions.delivery_state
// names exactly these.
const (
	DeliveryNone          = "none"
	DeliveryPending       = "pending"
	DeliveryDelivered     = "delivered"
	DeliveryUndeliverable = "undeliverable"
)

// DeliveryStates lists every delivery state, for the schema check test.
var DeliveryStates = []string{DeliveryNone, DeliveryPending, DeliveryDelivered, DeliveryUndeliverable}

// The card's answers. Any answer other than Approve is a denial; its text is the note.
const (
	AnswerApprove = "Approve"
	AnswerDeny    = "Deny"
)

// QueueIDPrefix starts the Needs-you queue item id of an action's card; the rest is the action id.
const QueueIDPrefix = "privileged_action_"

// Limits.
const (
	MaxRequestIDLen = 100
	MaxReasonLen    = 2000
	// MaxPendingPerSession and MaxPendingTotal bound undecided requests: each costs owner attention.
	MaxPendingPerSession = 3
	MaxPendingTotal      = 10
	// UndecidedExpiry is how long a request waits for the owner.
	UndecidedExpiry = 24 * time.Hour
	// UnclaimedExpiry is how long an approved action waits for the executor before it expires,
	// so a backlog never runs old approvals late.
	UnclaimedExpiry = 5 * time.Minute
	// MaxPreflightAttempts is how often a preflight that could not reach GitHub is tried.
	MaxPreflightAttempts = 5
)

var (
	// ErrInvalid is a request that breaks the contract; the message says how.
	ErrInvalid = errors.New("privileged: invalid request")
	// ErrConflict is a request id reused for a different action.
	ErrConflict = errors.New("privileged: request_id was used for a different action")
	// ErrTooManyPending is a request over the pending limits.
	ErrTooManyPending = errors.New("privileged: too many undecided requests")
	// ErrNotFound is an action the caller cannot see.
	ErrNotFound = errors.New("privileged: no such action")
	// ErrNotCancellable is a cancel of an action the owner already decided.
	ErrNotCancellable = errors.New("privileged: the action is no longer waiting for the owner")
	// ErrUnsupported is a type outside the catalog.
	ErrUnsupported = errors.New("privileged: unsupported action")
)

// Requester is who asks, resolved by the server from MCP authentication.
type Requester struct {
	Role      string
	SessionID string
	TaskID    *string
}

// Action is a stored action as the tools report it.
type Action struct {
	ID           string
	RequestID    string
	Type         string
	State        string
	Args         json.RawMessage
	Reason       string
	Facts        json.RawMessage
	Digest       string
	ExpiresAt    time.Time
	DecisionNote *string
	Result       json.RawMessage
	LastError    *string
	CreatedAt    time.Time
	FinishedAt   *time.Time
	QueueItemID  string
	// Replayed is set when the call returned an action made earlier: the same request id, or an
	// undecided action of the same session with the same arguments.
	Replayed bool
}

// Service takes requests. Its GitHub reads run the preflight.
type Service struct {
	Pool    *pgxpool.Pool
	OwnerID string
	GitHub  GitHub
	Log     *slog.Logger
	Now     func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Request validates and canonicalizes the request, returns the action already made under
// requestID or with the same arguments, and otherwise writes the intent row, runs the preflight
// with no transaction open and, when it passes, freezes the facts and opens the owner's card.
func (s *Service) Request(ctx context.Context, by Requester, typ string, rawArgs json.RawMessage, reason, requestID string) (Action, error) {
	if s.GitHub == nil {
		return Action{}, fmt.Errorf("%w: this Mainloop has no GitHub App", ErrUnsupported)
	}
	entry, ok := catalog[typ]
	if !ok {
		return Action{}, fmt.Errorf("%w: %q is not in the catalog (%s)", ErrUnsupported, typ, strings.Join(Types(), ", "))
	}
	args, err := entry.normalize(rawArgs)
	if err != nil {
		return Action{}, err
	}
	if requestID = strings.TrimSpace(requestID); !plainLine(requestID, MaxRequestIDLen) {
		return Action{}, fmt.Errorf("%w: request_id must be one line of 1 to %d characters", ErrInvalid, MaxRequestIDLen)
	}
	if reason = strings.TrimSpace(reason); reason == "" || utf8.RuneCountInString(reason) > MaxReasonLen {
		return Action{}, fmt.Errorf("%w: reason must be 1 to %d characters", ErrInvalid, MaxReasonLen)
	}
	argsJSON, err := canonical(args)
	if err != nil {
		return Action{}, err
	}
	argsKey := hashHex(append([]byte(typ+"\x00"), argsJSON...))
	q := dbq.New(s.Pool)
	if a, ok, err := s.existing(ctx, q, by.SessionID, requestID, typ, argsJSON, argsKey); ok || err != nil {
		return a, err
	}

	var row dbq.PrivilegedAction
	inserted := false
	err = events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		tq := dbq.New(tx)
		if err := tq.LockPrivilegedRequests(ctx, s.OwnerID); err != nil {
			return nil, err
		}
		n, err := tq.CountUndecidedPrivilegedActions(ctx, dbq.CountUndecidedPrivilegedActionsParams{OwnerID: s.OwnerID, SessionID: by.SessionID})
		if err != nil {
			return nil, err
		}
		if n.SessionCount >= MaxPendingPerSession || n.OwnerCount >= MaxPendingTotal {
			return nil, fmt.Errorf("%w: at most %d per session and %d in all wait for the owner; wait for an outcome or cancel one",
				ErrTooManyPending, MaxPendingPerSession, MaxPendingTotal)
		}
		r, err := tq.InsertPrivilegedAction(ctx, dbq.InsertPrivilegedActionParams{
			ID: "pa_" + uuid.NewString(), OwnerID: s.OwnerID, RequestID: requestID, RequesterRole: by.Role,
			RequesterSessionID: by.SessionID, RequesterTaskID: by.TaskID, Type: typ, Version: entry.version,
			Args: argsJSON, ArgsKey: argsKey, Reason: reason,
			// Stored at the database's precision, so the digest reads back the same instant.
			ExpiresAt: s.now().Add(entry.expiry).UTC().Truncate(time.Microsecond),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // a concurrent request won; read it below
		}
		if err != nil {
			return nil, err
		}
		row, inserted = r, true
		return nil, tq.InsertPrivilegedActionEvent(ctx, dbq.InsertPrivilegedActionEventParams{
			ActionID: r.ID, Step: "requested", Detail: fmt.Sprintf("%s requested %s by session %s", by.Role, typ, by.SessionID),
		})
	})
	if err != nil {
		return Action{}, err
	}
	if !inserted {
		if a, ok, err := s.existing(ctx, q, by.SessionID, requestID, typ, argsJSON, argsKey); ok || err != nil {
			return a, err
		}
		return Action{}, errors.New("privileged: the request conflicted but no action was found")
	}
	s.log().InfoContext(ctx, "privileged action requested", "action_id", row.ID, "type", typ, "role", by.Role)
	row, err = s.preflight(ctx, row, false)
	if err != nil {
		return Action{}, err
	}
	return fromRow(row), nil
}

// existing returns the action this call repeats: the one made under requestID (a different type
// or arguments is ErrConflict), else the session's undecided one with the same arguments.
func (s *Service) existing(ctx context.Context, q *dbq.Queries, sessionID, requestID, typ string, argsJSON []byte, argsKey string) (Action, bool, error) {
	row, err := q.GetPrivilegedActionByRequest(ctx, dbq.GetPrivilegedActionByRequestParams{SessionID: sessionID, RequestID: requestID})
	if err == nil {
		stored, err := canonicalRaw(row.Args)
		if err != nil {
			return Action{}, true, err
		}
		if row.Type != typ || string(stored) != string(argsJSON) {
			return Action{}, true, ErrConflict
		}
		a := fromRow(row)
		a.Replayed = true
		return a, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Action{}, true, err
	}
	row, err = q.GetOpenPrivilegedActionByArgs(ctx, dbq.GetOpenPrivilegedActionByArgsParams{SessionID: sessionID, ArgsKey: argsKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return Action{}, false, nil
	}
	if err != nil {
		return Action{}, true, err
	}
	a := fromRow(row)
	a.Replayed = true
	return a, true, nil
}

// preflight reads the facts of a requested action (no transaction open) and either freezes them
// and opens the card, or records why the action cannot go to the owner. A GitHub read that fails
// for a reason other than the target leaves the row requested for the sweep to try again, until
// MaxPreflightAttempts. deliver says whether a refusal is sent to the session as a message: the
// requesting call reports it itself; a sweep must deliver it.
func (s *Service) preflight(ctx context.Context, row dbq.PrivilegedAction, deliver bool) (dbq.PrivilegedAction, error) {
	entry, ok := catalog[row.Type]
	if !ok {
		return s.failPreflight(ctx, row, "the type is no longer in the catalog", deliver)
	}
	facts, err := entry.preflight(ctx, s.GitHub, row.Args, s.now().UTC())
	var refused *refusal
	switch {
	case errors.As(err, &refused):
		return s.failPreflight(ctx, row, refused.msg, deliver)
	case err != nil:
		msg := "GitHub could not be read: " + safeError(err)
		if row.PreflightAttempts+1 >= MaxPreflightAttempts {
			return s.failPreflight(ctx, row, msg, deliver)
		}
		if _, err := dbq.New(s.Pool).BumpPrivilegedPreflight(ctx, dbq.BumpPrivilegedPreflightParams{ID: row.ID, LastError: &msg}); err != nil {
			return row, err
		}
		row.PreflightAttempts++
		row.LastError = &msg
		return row, nil
	}
	factsJSON, err := canonical(facts)
	if err != nil {
		return row, err
	}
	digest, err := Digest(row, factsJSON)
	if err != nil {
		return row, err
	}
	var out dbq.PrivilegedAction
	err = events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		tq := dbq.New(tx)
		r, err := tq.FreezePrivilegedAction(ctx, dbq.FreezePrivilegedActionParams{ID: row.ID, FrozenFacts: factsJSON, Digest: &digest})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // cancelled, expired or frozen by another preflight meanwhile
		}
		if err != nil {
			return nil, err
		}
		out = r
		if err := tq.InsertPrivilegedActionEvent(ctx, dbq.InsertPrivilegedActionEventParams{
			ActionID: r.ID, Step: "preflighted", Detail: "facts frozen under digest " + digest,
		}); err != nil {
			return nil, err
		}
		item, err := card(r)
		if err != nil {
			return nil, err
		}
		return attention.Open(ctx, tx, s.OwnerID, item)
	})
	if err != nil {
		return row, err
	}
	if out.ID == "" {
		return s.reread(ctx, row.ID)
	}
	s.log().InfoContext(ctx, "privileged action awaits the owner", "action_id", out.ID, "type", out.Type)
	return out, nil
}

func (s *Service) failPreflight(ctx context.Context, row dbq.PrivilegedAction, msg string, deliver bool) (dbq.PrivilegedAction, error) {
	var out dbq.PrivilegedAction
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tq := dbq.New(tx)
		r, err := tq.FailPrivilegedPreflight(ctx, dbq.FailPrivilegedPreflightParams{ID: row.ID, LastError: &msg, Deliver: deliver})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = r
		return tq.InsertPrivilegedActionEvent(ctx, dbq.InsertPrivilegedActionEventParams{ActionID: r.ID, Step: "preflight_failed", Detail: msg})
	})
	if err != nil {
		return row, err
	}
	if out.ID == "" {
		return s.reread(ctx, row.ID)
	}
	return out, nil
}

func (s *Service) reread(ctx context.Context, id string) (dbq.PrivilegedAction, error) {
	return dbq.New(s.Pool).GetPrivilegedAction(ctx, dbq.GetPrivilegedActionParams{OwnerID: s.OwnerID, ID: id})
}

// Get returns an action the session requested. Another session's action is ErrNotFound.
func (s *Service) Get(ctx context.Context, sessionID, id string) (Action, []dbq.PrivilegedActionEvent, error) {
	row, err := s.reread(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.RequesterSessionID != sessionID) {
		return Action{}, nil, ErrNotFound
	}
	if err != nil {
		return Action{}, nil, err
	}
	evs, err := dbq.New(s.Pool).ListPrivilegedActionEvents(ctx, row.ID)
	if err != nil {
		return Action{}, nil, err
	}
	return fromRow(row), evs, nil
}

// Cancel withdraws an undecided action of the session and closes its card.
func (s *Service) Cancel(ctx context.Context, sessionID, id string) (Action, error) {
	var out dbq.PrivilegedAction
	err := events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		tq := dbq.New(tx)
		r, err := tq.CancelPrivilegedAction(ctx, dbq.CancelPrivilegedActionParams{OwnerID: s.OwnerID, ID: id, SessionID: sessionID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		out = r
		if err := tq.InsertPrivilegedActionEvent(ctx, dbq.InsertPrivilegedActionEventParams{ActionID: r.ID, Step: "cancelled", Detail: "the requesting session withdrew it"}); err != nil {
			return nil, err
		}
		return resolveCards(ctx, tq, s.OwnerID)
	})
	if err != nil {
		return Action{}, err
	}
	if out.ID != "" {
		return fromRow(out), nil
	}
	a, _, err := s.Get(ctx, sessionID, id)
	if err != nil {
		return Action{}, err
	}
	if a.State == StateCancelled {
		return a, nil
	}
	return a, ErrNotCancellable
}

func fromRow(r dbq.PrivilegedAction) Action {
	a := Action{
		ID: r.ID, RequestID: r.RequestID, Type: r.Type, State: r.State, Args: r.Args, Reason: r.Reason,
		Facts: r.FrozenFacts, ExpiresAt: r.ExpiresAt, DecisionNote: r.DecisionNote, Result: r.Result,
		LastError: r.LastError, CreatedAt: r.CreatedAt, FinishedAt: r.FinishedAt, QueueItemID: QueueIDPrefix + r.ID,
	}
	if r.Digest != nil {
		a.Digest = *r.Digest
	}
	return a
}

func plainLine(s string, maxLen int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= maxLen && !strings.ContainsFunc(s, unicode.IsControl)
}
