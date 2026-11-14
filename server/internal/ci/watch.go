package ci

// Check watches: an agent asks to be told when CI on one commit settles, and ends its turn. Its
// workspace can then suspend while CI runs instead of holding a turn (and its memory) open.
//
// watch_checks writes a check_watches row and returns at once. The watch loop claims the row, reads
// the checks with no transaction open, and either polls again later or settles: it records the
// outcome and the rendered message (watching -> delivering), then delivers the message to the
// session under the watch's id. That send is idempotent on the id, so a delivery whose outcome is
// unknown is simply sent again; it steers into a running turn or queues as the next one, like any
// other message from Mainloop.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// WatchChannel wakes the watch loop when a watch is created.
const WatchChannel = "mainloop_check_watches"

// Watch timing.
const (
	// DefaultWatchTimeout is how long a watch waits for CI when the caller names no timeout, and
	// MaxWatchTimeout the longest it may ask for.
	DefaultWatchTimeout = 30 * time.Minute
	MaxWatchTimeout     = 2 * time.Hour
	// noChecksGrace is how long a commit may show no checks at all before the watch settles as
	// no_checks: CI takes a moment to register its runs after a push.
	noChecksGrace = 3 * time.Minute
	// deliveryGrace is how long after the deadline (SessionAwaitsCheckWatch, which keeps the task
	// liveness sweep from reading a waiting worker as stalled, uses the same 30 minutes) a message that cannot be delivered yet (the
	// workspace is not ready, say) is retried before the watch gives up on it.
	deliveryGrace = 30 * time.Minute
)

// pollDelay is the wait before the next read of a watch created elapsed ago: often while a quick
// CI may finish, then less often.
func pollDelay(elapsed time.Duration) time.Duration {
	switch {
	case elapsed < 2*time.Minute:
		return 15 * time.Second
	case elapsed < 10*time.Minute:
		return 30 * time.Second
	}
	return time.Minute
}

// Sender delivers a message to a session: the session service.
type Sender interface {
	SendToWith(ctx context.Context, source sessions.Source, sessionID, messageID, text string, o sessions.SendOptions) (sessions.Sent, error)
}

// WatchTable is the watch ledger as an intent table.
func WatchTable() reconcile.Table {
	return reconcile.NewTable("check_watches", string(dbstate.WatchWatching), string(dbstate.WatchDelivering))
}

// WatchLoop polls watches and delivers their outcome. It needs a [Sender].
func (s *Service) WatchLoop() reconcile.Loop {
	return reconcile.Loop{Name: "check-watches", Table: WatchTable(), Handle: s.handleWatch, Wake: []string{WatchChannel}}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// WatchInput is one watch_checks call.
type WatchInput struct {
	OwnerID, ProjectID, SessionID string
	Ref                           Ref
	// RequestID makes the call idempotent per session: a retry finds the same watch.
	RequestID string
	// Timeout is how long to wait for CI; zero means [DefaultWatchTimeout].
	Timeout time.Duration
}

// Watch is a recorded watch.
type Watch struct {
	ID       string
	State    dbstate.CheckWatchState
	Deadline time.Time
	// Superseded counts the older watches of the same ref this one replaced.
	Superseded int64
}

// StartWatch records a watch and returns at once; the watch loop does the waiting. A newer watch of
// the same session and ref replaces older ones still watching, so a push-and-watch again sends one
// message, about the latest. A retried call with the same request id returns the same watch.
func (s *Service) StartWatch(ctx context.Context, in WatchInput) (Watch, error) {
	if !in.Ref.Valid() {
		return Watch{}, &Refusal{"invalid_ref", "give exactly one of pr_number or a 40-character commit_sha"}
	}
	if in.Timeout == 0 {
		in.Timeout = DefaultWatchTimeout
	}
	if in.Timeout < time.Minute || in.Timeout > MaxWatchTimeout {
		return Watch{}, &Refusal{"invalid_timeout", fmt.Sprintf("timeout must be between 1 and %d minutes", int(MaxWatchTimeout.Minutes()))}
	}
	if in.SessionID == "" || in.RequestID == "" {
		return Watch{}, &Refusal{"invalid_request", "a watch needs a session and a request_id"}
	}
	if _, err := s.project(ctx, in.OwnerID, in.ProjectID); err != nil {
		return Watch{}, err
	}
	var pr *int32
	var sha *string
	if in.Ref.PR > 0 {
		n := int32(min(in.Ref.PR, 1<<31-1)) //nolint:gosec // bounded just above
		pr = &n
	} else {
		sha = &in.Ref.SHA
	}
	key := in.SessionID + "\x1f" + in.RequestID
	var w Watch
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := db.XactLock(ctx, tx, "mainloop:check-watch:"+in.SessionID); err != nil {
			return err
		}
		row, err := q.GetCheckWatchByKey(ctx, key)
		if err == nil {
			if row.ProjectID != in.ProjectID || !sameRef(row, pr, sha) {
				return &Refusal{"request_id_reused", "this request_id already started a different watch"}
			}
			w = Watch{ID: row.ID, State: dbstate.CheckWatchState(row.State), Deadline: row.Deadline}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		row, err = q.InsertCheckWatch(ctx, dbq.InsertCheckWatchParams{
			ID: uuid.NewString(), IdempotencyKey: key, UserID: in.OwnerID, ProjectID: in.ProjectID,
			SessionID: in.SessionID, PrNumber: pr, CommitSha: sha, Deadline: s.now().Add(in.Timeout),
		})
		if err != nil {
			return fmt.Errorf("record watch: %w", err)
		}
		n, err := q.SupersedeCheckWatches(ctx, dbq.SupersedeCheckWatchesParams{SessionID: in.SessionID, ID: row.ID, PrNumber: pr, CommitSha: sha})
		if err != nil {
			return fmt.Errorf("supersede watches: %w", err)
		}
		w = Watch{ID: row.ID, State: dbstate.CheckWatchState(row.State), Deadline: row.Deadline, Superseded: n}
		return q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: WatchChannel, Payload: row.ID})
	})
	return w, err
}

func sameRef(row dbq.CheckWatch, pr *int32, sha *string) bool {
	switch {
	case pr != nil:
		return row.PrNumber != nil && *row.PrNumber == *pr
	case sha != nil:
		return row.CommitSha != nil && *row.CommitSha == *sha
	}
	return false
}

func refOf(row dbq.CheckWatch) Ref {
	if row.PrNumber != nil {
		return Ref{PR: int(*row.PrNumber)}
	}
	return Ref{SHA: *row.CommitSha}
}

func (s *Service) handleWatch(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	row, err := dbq.New(s.Pool).GetCheckWatch(ctx, c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), nil
	}
	if err != nil {
		return reconcile.Retry(), fmt.Errorf("load watch: %w", err)
	}
	if dbstate.CheckWatchState(c.State) == dbstate.WatchDelivering {
		return s.deliver(ctx, c, row)
	}
	return s.poll(ctx, c, row)
}

// poll reads the checks once and settles the watch, or comes back later.
func (s *Service) poll(ctx context.Context, c reconcile.Claim, row dbq.CheckWatch) (reconcile.Outcome, error) {
	now := s.now()
	elapsed := now.Sub(row.CreatedAt)
	late := !now.Before(row.Deadline)
	st, err := s.Status(ctx, row.UserID, row.ProjectID, refOf(row))
	var r *Refusal
	switch {
	case errors.As(err, &r) && r.Code == "not_found":
		return s.settle(ctx, c, row, dbstate.WatchNotFound, "", notFoundMessage(row, r.Message))
	case errors.Is(err, ErrProjectNotFound):
		return s.finish(ctx, c, dbstate.WatchUndeliverable, "the project is gone")
	case err != nil && late:
		return s.settle(ctx, c, row, dbstate.WatchPendingTimeout, "", timeoutMessage(row, nil, err))
	case err != nil:
		return reconcile.RetryAfter(pollDelay(elapsed)), err
	}
	settled := st.Counts.Pending == 0 && (st.State != NoChecks || elapsed >= noChecksGrace)
	switch {
	case settled:
		return s.settle(ctx, c, row, dbstate.CheckWatchOutcome(st.State), st.SHA, resultMessage(row, st))
	case late && st.State == NoChecks:
		return s.settle(ctx, c, row, dbstate.WatchNoChecks, st.SHA, resultMessage(row, st))
	case late:
		return s.settle(ctx, c, row, dbstate.WatchPendingTimeout, st.SHA, timeoutMessage(row, &st, nil))
	}
	return reconcile.RetryAfter(min(pollDelay(elapsed), row.Deadline.Sub(now))), nil
}

// settle records the outcome and message, then delivers under the same claim.
func (s *Service) settle(ctx context.Context, c reconcile.Claim, row dbq.CheckWatch, outcome dbstate.CheckWatchOutcome, sha, message string) (reconcile.Outcome, error) {
	var head *string
	if sha != "" {
		head = &sha
	}
	owner, out := c.Owner, string(outcome)
	n, err := dbq.New(s.Pool).SettleCheckWatch(ctx, dbq.SettleCheckWatchParams{
		ID: c.ID, LeaseOwner: &owner, Attempts: int32(c.Attempts), //nolint:gosec // a claim count
		Outcome: &out, HeadSha: head, Message: &message,
	})
	if err != nil {
		return reconcile.Retry(), fmt.Errorf("settle watch: %w", err)
	}
	if n == 0 {
		// Superseded meanwhile, or the lease was lost: nothing of ours to deliver.
		return reconcile.Recorded(), nil
	}
	row.Message = &message
	return s.deliver(ctx, c, row)
}

// deliver sends the recorded message to the session, once: the send dedups on the watch id.
func (s *Service) deliver(ctx context.Context, c reconcile.Claim, row dbq.CheckWatch) (reconcile.Outcome, error) {
	if s.Sender == nil {
		return reconcile.RetryAfter(time.Minute), errors.New("ci: no sender for watch delivery")
	}
	if row.Message == nil {
		return s.finish(ctx, c, dbstate.WatchUndeliverable, "no message was recorded")
	}
	_, err := s.Sender.SendToWith(ctx, sessions.SourceMainloop, row.SessionID, row.ID, *row.Message, sessions.SendOptions{Steer: true})
	switch {
	case err == nil:
		return s.finish(ctx, c, dbstate.WatchDelivered, "")
	case errors.Is(err, sessions.ErrBusy):
		return reconcile.RetryAfter(3 * time.Second), nil
	case errors.Is(err, sessions.ErrNotFound):
		return s.finish(ctx, c, dbstate.WatchUndeliverable, "the session is gone")
	case errors.Is(err, sessions.ErrConflict):
		// The session ended, was archived, or is not ready yet: retry for a while, then give up.
		if s.now().After(row.Deadline.Add(deliveryGrace)) {
			return s.finish(ctx, c, dbstate.WatchUndeliverable, err.Error())
		}
		return reconcile.RetryAfter(30 * time.Second), nil
	}
	return reconcile.RetryAfter(10 * time.Second), err
}

func (s *Service) finish(ctx context.Context, c reconcile.Claim, state dbstate.CheckWatchState, detail string) (reconcile.Outcome, error) {
	var d *string
	if detail != "" {
		d = &detail
	}
	owner := c.Owner
	n, err := dbq.New(s.Pool).FinishCheckWatch(ctx, dbq.FinishCheckWatchParams{
		ID: c.ID, LeaseOwner: &owner, Attempts: int32(c.Attempts), //nolint:gosec // a claim count
		State: string(state), Detail: d,
	})
	if err != nil {
		return reconcile.Retry(), fmt.Errorf("finish watch: %w", err)
	}
	if n == 0 {
		return reconcile.Recorded(), reconcile.ErrLeaseLost
	}
	return reconcile.Recorded(), nil
}

func refText(row dbq.CheckWatch) string {
	if row.PrNumber != nil {
		return fmt.Sprintf("PR #%d", *row.PrNumber)
	}
	return "commit " + (*row.CommitSha)[:12]
}

const watchHeader = "[Mainloop] watch_checks result for %s (watch %s)\n"

func resultMessage(row dbq.CheckWatch, st Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, watchHeader, refText(row), row.ID)
	b.WriteString(Render(st))
	switch st.State {
	case Green:
		b.WriteString("\n\nEvery check passed.")
	case Red:
		b.WriteString("\n\nFix the failures above, push, and call watch_checks again.")
	case NoChecks:
		b.WriteString("\n\nNo CI reported a check on this commit. If the project has no CI for this change, " +
			"run its checks in the workspace instead.")
	}
	return b.String()
}

func timeoutMessage(row dbq.CheckWatch, st *Status, readErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, watchHeader, refText(row), row.ID)
	fmt.Fprintf(&b, "CI had not settled by the watch deadline (%s).\n", row.Deadline.UTC().Format(time.RFC3339))
	if st != nil {
		b.WriteString(Render(*st))
	} else if readErr != nil {
		var r *Refusal
		if errors.As(readErr, &r) {
			b.WriteString(r.Message)
		} else {
			b.WriteString("CI could not be read.")
		}
	}
	b.WriteString("\n\nCI may be stuck or slow. Use check_status for a fresh look, watch_checks again to keep waiting, " +
		"or run the checks in the workspace.")
	return b.String()
}

func notFoundMessage(row dbq.CheckWatch, why string) string {
	return fmt.Sprintf(watchHeader, refText(row), row.ID) + "Nothing to watch: " + why + "."
}
