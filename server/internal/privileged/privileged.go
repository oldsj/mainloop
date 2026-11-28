// Package privileged runs credentialed actions an agent requests and the owner approves from the
// phone (docs/specs/privileged-actions.md). An agent requests a catalog action; Mainloop
// canonicalizes its arguments, writes the intent row, runs a read-only preflight with no
// transaction open, freezes what it found under a digest and opens one approval card. The owner's
// Approve must carry that digest. The executor then runs exactly that action once, observing the
// remote before any retry, and the outcome goes back to the requesting session as one message.
//
// github.pr_merge and github.pr_close run in serve-api through Mainloop's own GitHub App; nothing
// bypasses a branch rule. onepassword.item_to_secret and the k8s.* types run in serve-executor, the
// only process that holds the 1Password service account and may mint Kubernetes credentials: it
// preflights and runs executor types, and serve-api opens their cards, records decisions and
// delivers outcomes.
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

	"github.com/oldsj/mainloop/server/internal/approval"
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

// The channel an approval came through, recorded with it (privileged_actions.approval_channel):
// the web app, whose answer carries no signature, or a phone's, signed with Touch ID.
const (
	ChannelWeb         = "web"
	ChannelPhoneSigned = "phone-signed"
)

// Channels lists every approval channel, for the schema check test.
var Channels = []string{ChannelWeb, ChannelPhoneSigned}

// ChannelText is a channel in words, for the agent's message and view: how the owner approved.
func ChannelText(channel string) string {
	switch channel {
	case ChannelWeb:
		return "from the web app (no Touch ID)"
	case ChannelPhoneSigned:
		return "on their phone, signed with Touch ID"
	}
	return ""
}

// The card's answers. Any answer other than Approve is a denial; its text is the note.
const (
	AnswerApprove = "Approve"
	AnswerDeny    = "Deny"
)

// QueueIDPrefix starts the Needs-you queue item id of an action's card, and UncertainIDPrefix the
// owner's error item for an outcome Mainloop could not observe; the rest is the action id.
const (
	QueueIDPrefix     = "privileged_action_"
	UncertainIDPrefix = "privileged_action_uncertain_"
)

// Limits.
const (
	MaxRequestIDLen = 100
	MaxReasonLen    = 2000
	// MaxPendingPerSession and MaxPendingTotal bound undecided requests: each costs owner attention.
	MaxPendingPerSession = 3
	// MaxRefusedPerHour bounds a session's requests of the executor's types (the cluster and
	// 1Password targets) that a preflight refused in the last hour. A refusal reaches no card, and
	// whether a request is refused, and why, says something about the target: unbounded, a session
	// could probe it, guess by guess, without the owner seeing. GitHub merges and closes are not
	// counted: their refusals (a moved head, a closed PR) say nothing the session cannot read on
	// GitHub, and main must not lock itself out of merging.
	MaxRefusedPerHour = 5
	MaxPendingTotal   = 10
	// UndecidedExpiry is how long a request waits for the owner.
	UndecidedExpiry = 24 * time.Hour
	// UnclaimedExpiry is how long an approved action waits for the executor before it expires,
	// so a backlog never runs old approvals late.
	UnclaimedExpiry = 5 * time.Minute
	// MaxPreflightAttempts is how often a preflight that could not read its target is tried.
	MaxPreflightAttempts = 5
	// ExecutorPickup is how long an executor type waits for serve-executor's preflight before
	// serve-api ends it: the executor is down or not deployed.
	ExecutorPickup = 10 * time.Minute
)

var (
	// ErrInvalid is a request that breaks the contract; the message says how.
	ErrInvalid = errors.New("privileged: invalid request")
	// ErrConflict is a request id reused for a different action.
	ErrConflict = errors.New("privileged: request_id was used for a different action")
	// ErrTooManyPending is a request over the pending limits.
	ErrTooManyPending = errors.New("privileged: too many undecided requests")
	// ErrTooManyRefused is a request of a session whose requests were refused too often lately.
	ErrTooManyRefused = errors.New("privileged: too many refused requests")
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
	// ApprovalChannel is how the owner approved (ChannelWeb or ChannelPhoneSigned); nil until then.
	ApprovalChannel *string
	Result          json.RawMessage
	LastError       *string
	CreatedAt       time.Time
	FinishedAt      *time.Time
	QueueItemID     string
	// Replayed is set when the call returned an action made earlier: the same request id, or an
	// undecided action of the same session with the same arguments.
	Replayed bool
}

// Service takes requests and runs preflights through its clients. serve-api's has GitHub;
// serve-executor's (Executor set) has OnePassword and Secrets.
type Service struct {
	Pool    *pgxpool.Pool
	OwnerID string
	GitHub  GitHub
	Log     *slog.Logger
	Now     func() time.Time
	// ExecutorOn, in serve-api, accepts executor types: a serve-executor is deployed.
	ExecutorOn bool
	// KubernetesOn, in serve-api, accepts the k8s.* types: the executor has Kubernetes access.
	KubernetesOn bool
	// Executor marks serve-executor's service: it freezes facts without opening the card, which
	// serve-api opens.
	Executor         bool
	OnePassword      OnePassword
	Secrets          Secrets
	SecretNamespaces []string
	// Kube is serve-executor's access for the k8s.* types; nil without it.
	Kube Kube
	// ExecutorNamespace is serve-executor's own namespace: the k8s.* types never change it.
	ExecutorNamespace string
	// Cluster names the cluster the executor acts on, for the card.
	Cluster string
	// Kubectl runs command.kubectl as AccountAdmin; nil without it.
	Kubectl Kubectl
	// OmniOn, in serve-api, accepts command.omnictl and command.talosctl: the executor has Omni
	// access.
	OmniOn bool
	// Sidero runs omnictl and talosctl through Omni; nil without Omni access.
	Sidero Sidero
	// OmniClusters are the clusters command.talosctl may act on.
	OmniClusters []string
	// ApprovalKeys, in serve-executor, are the pinned approval keys. With any, an action runs only
	// when one of them signed its approval for its id and digest.
	ApprovalKeys approval.Pinned
	// ApprovalsRequired, in serve-executor, refuses every action without a pinned key's signature,
	// even with no key pinned (fail closed).
	ApprovalsRequired bool
	// SSHOn, in serve-api, accepts command.ssh: the executor reaches home-network hosts over ssh.
	SSHOn bool
	// SSH reaches the allowlisted hosts; nil without ssh access.
	SSH SSH
	// SSHHosts are the host names command.ssh may target.
	SSHHosts []string
	// UniFiOn, in serve-api, accepts http.unifi: the executor reaches the UniFi console.
	UniFiOn bool
	// UniFi reaches the UniFi console; nil without UniFi access.
	UniFi UniFi
	// ToolboxOn, in serve-api, accepts command.exec: the executor runs scripts in the toolbox.
	ToolboxOn bool
	// Toolbox makes and reads the toolbox pods; nil without a toolbox.
	Toolbox Toolbox
	// ToolboxProxyImage is the image of the kube proxy sidecar (the executor's own, by digest):
	// set, a run may have cluster reach.
	ToolboxProxyImage string
	// ToolboxImage is the toolbox image, pinned by digest: what a run's pod uses, and what its
	// card froze.
	ToolboxImage string
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
	entry, ok := catalog[typ]
	if !ok {
		return Action{}, fmt.Errorf("%w: %q is not in the catalog (%s)", ErrUnsupported, typ, strings.Join(Types(), ", "))
	}
	switch {
	case entry.executor && !s.ExecutorOn:
		return Action{}, fmt.Errorf("%w: %s needs Mainloop's executor, which this Mainloop does not run", ErrUnsupported, typ)
	case entry.kube && !s.KubernetesOn:
		return Action{}, fmt.Errorf("%w: %s needs Kubernetes access for Mainloop's executor, which this Mainloop does not have", ErrUnsupported, typ)
	case entry.omni && !s.OmniOn:
		return Action{}, fmt.Errorf("%w: %s needs Omni access for Mainloop's executor, which this Mainloop does not have", ErrUnsupported, typ)
	case entry.ssh && !s.SSHOn:
		return Action{}, fmt.Errorf("%w: %s needs ssh access for Mainloop's executor, which this Mainloop does not have", ErrUnsupported, typ)
	case entry.toolbox && !s.ToolboxOn:
		return Action{}, fmt.Errorf("%w: %s needs a toolbox for Mainloop's executor, which this Mainloop does not have", ErrUnsupported, typ)
	case entry.unifi && !s.UniFiOn:
		return Action{}, fmt.Errorf("%w: %s needs UniFi access for Mainloop's executor, which this Mainloop does not have", ErrUnsupported, typ)
	case !entry.executor && s.GitHub == nil:
		return Action{}, fmt.Errorf("%w: this Mainloop has no GitHub App", ErrUnsupported)
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
	expiry := entry.expiry
	if entry.expiryFor != nil {
		expiry = entry.expiryFor(args)
	}
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
		var refused int32
		if entry.executor {
			if refused, err = tq.CountRefusedPrivilegedPreflights(ctx, dbq.CountRefusedPrivilegedPreflightsParams{
				OwnerID: s.OwnerID, SessionID: by.SessionID, Types: TypesFor(true),
			}); err != nil {
				return nil, err
			}
		}
		if refused >= MaxRefusedPerHour {
			return nil, fmt.Errorf("%w: %d requests of this session were refused at preflight in the last hour; "+
				"stop and report blocked with what you meant to do, for the owner", ErrTooManyRefused, MaxRefusedPerHour)
		}
		r, err := tq.InsertPrivilegedAction(ctx, dbq.InsertPrivilegedActionParams{
			ID: "pa_" + uuid.NewString(), OwnerID: s.OwnerID, RequestID: requestID, RequesterRole: by.Role,
			RequesterSessionID: by.SessionID, RequesterTaskID: by.TaskID, Type: typ, Version: entry.version,
			Args: argsJSON, ArgsKey: argsKey, Reason: reason,
			// Stored at the database's precision, so the digest reads back the same instant.
			ExpiresAt: s.now().Add(expiry).UTC().Truncate(time.Microsecond),
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
	if entry.executor {
		return fromRow(row), nil // serve-executor preflights it; its card opens when it has
	}
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
		return s.failPreflight(ctx, row, "preflight_failed", "the type is no longer in the catalog", deliver)
	}
	rctx, cancel := context.WithTimeout(ctx, RemoteTimeout)
	facts, err := entry.preflight(rctx, s, row.Args, s.now().UTC())
	cancel()
	var refused *refusal
	switch {
	case errors.As(err, &refused):
		return s.failPreflight(ctx, row, "preflight_refused", refused.msg, deliver)
	case err != nil:
		msg := "the target could not be read: " + safeError(err)
		if row.PreflightAttempts+1 >= MaxPreflightAttempts {
			return s.failPreflight(ctx, row, "preflight_failed", msg, deliver)
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
		if s.Executor {
			return nil, nil // serve-api opens the card; the executor writes no attention
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

// failPreflight ends the action as preflight_failed. step is the audit event's: preflight_refused
// for a target the action cannot apply to (counted against MaxRefusedPerHour), else
// preflight_failed.
func (s *Service) failPreflight(ctx context.Context, row dbq.PrivilegedAction, step, msg string, deliver bool) (dbq.PrivilegedAction, error) {
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
		return tq.InsertPrivilegedActionEvent(ctx, dbq.InsertPrivilegedActionEventParams{ActionID: r.ID, Step: step, Detail: msg})
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
		Facts: r.FrozenFacts, ExpiresAt: r.ExpiresAt, DecisionNote: r.DecisionNote, ApprovalChannel: r.ApprovalChannel, Result: r.Result,
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
