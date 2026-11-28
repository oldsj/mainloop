package privileged

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// Executor tuning.
const (
	// MaxAttempts is how many claims an approved action gets. Every attempt after the first starts
	// by observing whether an earlier one already acted.
	MaxAttempts = 5
	// MaxDeliveryAttempts is how often an outcome is offered to its session.
	MaxDeliveryAttempts = 20
	execLease           = 2 * time.Minute
	retryDelay          = 30 * time.Second
	preflightIdle       = 30 * time.Second
	maxExecPerPass      = 3
	maxDeliveryPerPass  = 10
)

// Sender delivers an outcome to the requesting session: the session service.
type Sender interface {
	SendToWith(ctx context.Context, source sessions.Source, sessionID, messageID, text string, o sessions.SendOptions) (sessions.Sent, error)
}

// Runner runs in two processes. In serve-api it preflights and runs the GitHub types, opens every
// card, records decisions, expires what waited too long, raises uncertain outcomes to the owner and
// delivers outcomes. In serve-executor (Service.Executor) it only preflights and runs the executor
// types: it writes nothing but privileged_actions and its events.
type Runner struct {
	*Service
	Sender Sender
	// ToolboxPoll is how often a command.exec run's pod is read while it runs; 2 s when zero.
	ToolboxPoll time.Duration
	// ToolboxReap is how often the executor deletes the pods of finished actions; 30 s when zero,
	// every pass when negative.
	ToolboxReap time.Duration
	reapedAt    time.Time
}

// Sweep runs the runner every few seconds.
func (r *Runner) Sweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "privileged-actions", Every: 3 * time.Second, Run: r.Run}
}

// Run does one pass. Each step is its own short transaction or a claimed attempt; no transaction
// is open across a remote call.
func (r *Runner) Run(ctx context.Context) error {
	if r.Executor {
		if err := r.preflightStuck(ctx); err != nil {
			return err
		}
		if err := r.execute(ctx); err != nil {
			return err
		}
		return r.reapToolbox(ctx)
	}
	for _, step := range []func(context.Context) error{
		r.preflightStuck, r.openCards, r.failUnpicked, r.decide, r.expire, r.execute, r.raiseUncertain,
	} {
		if err := step(ctx); err != nil {
			return err
		}
	}
	return r.deliverAll(ctx)
}

// types are the catalog types this process runs.
func (r *Runner) types() []string { return TypesFor(r.Executor) }

// execute claims and runs this process's approved actions.
func (r *Runner) execute(ctx context.Context) error {
	for range maxExecPerPass {
		row, err := dbq.New(r.Pool).ClaimPrivilegedAction(ctx, dbq.ClaimPrivilegedActionParams{
			OwnerID: r.OwnerID, LeaseSeconds: execLease.Seconds(), Types: r.types(),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := r.attempt(ctx, row); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) deliverAll(ctx context.Context) error {
	for range maxDeliveryPerPass {
		row, err := dbq.New(r.Pool).ClaimPrivilegedDelivery(ctx, dbq.ClaimPrivilegedDeliveryParams{OwnerID: r.OwnerID, LeaseSeconds: 60})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := r.deliver(ctx, row); err != nil {
			return err
		}
	}
	return nil
}

// preflightStuck preflights this process's requested actions: in serve-api, those whose request
// call died or could not read GitHub; in serve-executor, every new one at once. A preflight that
// could not read its target is retried after preflightIdle.
func (r *Runner) preflightStuck(ctx context.Context) error {
	first := preflightIdle
	if r.Executor {
		first = 0 // no request call preflights an executor type
	}
	rows, err := dbq.New(r.Pool).ListUnpreflightedPrivilegedActions(ctx, dbq.ListUnpreflightedPrivilegedActionsParams{
		OwnerID: r.OwnerID, FirstSeconds: first.Seconds(), IdleSeconds: preflightIdle.Seconds(), Types: r.types(),
	})
	if err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := r.preflight(ctx, row, true); err != nil {
			return err
		}
	}
	return nil
}

// openCards opens the card of every frozen action that has none: an executor type serve-executor
// preflighted. (A GitHub type's card opens in the transaction that froze it.)
func (r *Runner) openCards(ctx context.Context) error {
	rows, err := dbq.New(r.Pool).ListUncardedPrivilegedActions(ctx, r.OwnerID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		item, err := card(row)
		if err != nil {
			return err
		}
		err = events.InTxPublish(ctx, r.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
			return attention.Open(ctx, tx, r.OwnerID, item)
		})
		if err != nil {
			return err
		}
		r.log().InfoContext(ctx, "privileged action awaits the owner", "action_id", row.ID, "type", row.Type)
	}
	return nil
}

// failUnpicked ends executor types serve-executor did not preflight in time: it is down or not
// deployed, and the agent should hear so rather than wait a day.
func (r *Runner) failUnpicked(ctx context.Context) error {
	return pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		msg := "Mainloop's executor did not preflight it within " + ExecutorPickup.String() + "; it may be down"
		ids, err := q.FailUnpickedPrivilegedActions(ctx, dbq.FailUnpickedPrivilegedActionsParams{
			OwnerID: r.OwnerID, Types: TypesFor(true), PickupSeconds: ExecutorPickup.Seconds(), LastError: &msg,
		})
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := q.InsertPrivilegedActionEvent(ctx, dbq.InsertPrivilegedActionEventParams{ActionID: id, Step: "preflight_failed", Detail: "the executor did not pick it up"}); err != nil {
				return err
			}
		}
		return nil
	})
}

// raiseUncertain gives the owner one error item per action whose outcome is unknown.
func (r *Runner) raiseUncertain(ctx context.Context) error {
	rows, err := dbq.New(r.Pool).ListUnraisedUncertainPrivilegedActions(ctx, r.OwnerID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		detail := ""
		if row.LastError != nil {
			detail = *row.LastError + " "
		}
		err := events.InTxPublish(ctx, r.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
			return attention.Open(ctx, tx, r.OwnerID, attention.NewQueueItem{
				ID: UncertainIDPrefix + row.ID, ItemType: api.QueueItemTypeError, Priority: api.QueueItemPriorityHigh,
				Title:   "Privileged action " + row.ID + ": outcome unknown",
				Content: "Mainloop could not tell whether " + row.Type + " ran. " + detail + "Check the target before asking again.",
			})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) decide(ctx context.Context) error {
	return pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		rows, err := q.DecideAnsweredPrivilegedActions(ctx, r.OwnerID)
		if err != nil {
			return err
		}
		for _, d := range rows {
			detail := "the owner approved through the owner API, with the card's digest"
			if d.State == StateDenied {
				detail = "the owner denied"
				if d.DecisionNote != nil {
					detail += ": " + *d.DecisionNote
				}
			}
			if err := q.InsertPrivilegedActionEvent(ctx, dbq.InsertPrivilegedActionEventParams{ActionID: d.ID, Step: "decided", Detail: detail}); err != nil {
				return err
			}
			r.log().InfoContext(ctx, "privileged action decided", "action_id", d.ID, "state", d.State)
		}
		return nil
	})
}

func (r *Runner) expire(ctx context.Context) error {
	return events.InTxPublish(ctx, r.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		rows, err := q.ExpirePrivilegedActions(ctx, dbq.ExpirePrivilegedActionsParams{OwnerID: r.OwnerID, ClaimSeconds: UnclaimedExpiry.Seconds()})
		if err != nil {
			return nil, err
		}
		for _, e := range rows {
			if err := q.InsertPrivilegedActionEvent(ctx, dbq.InsertPrivilegedActionEventParams{ActionID: e.ID, Step: "expired", Detail: "no decision or claim in time"}); err != nil {
				return nil, err
			}
		}
		return resolveCards(ctx, q, r.OwnerID)
	})
}

// resolveCards closes the cards of actions no longer waiting for the owner.
func resolveCards(ctx context.Context, q *dbq.Queries, ownerID string) ([]events.Draft, error) {
	ids, err := q.ResolvePrivilegedActionCards(ctx, ownerID)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	n, err := q.CountUnreadAttention(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	count := int(n)
	return []events.Draft{{UserID: ownerID, Type: events.TypeInboxUpdated, Payload: api.EventInboxUpdated{UnreadCount: &count}}}, nil
}

// outcome is how an attempt ended.
type outcome struct {
	state  string // a final state, or "" to retry
	result any
	err    string
}

func retry(err error) outcome { return outcome{err: safeError(err)} }

// attempt runs one claimed action. It recomputes the digest and refuses a mismatch before any
// remote call, then executes; a type's execute observes the remote first, so an attempt after a
// crash or a lost answer finishes rather than repeats.
func (r *Runner) attempt(ctx context.Context, row dbq.PrivilegedAction) error {
	var out outcome
	if want, err := Digest(row, row.FrozenFacts); err != nil || row.Digest == nil || row.ApprovedDigest == nil ||
		want != *row.Digest || *row.ApprovedDigest != want {
		out = outcome{state: StateFailed, err: "the stored action no longer matches the digest the owner approved; nothing ran"}
	} else if err := exactRow(row); err != nil {
		out = outcome{state: StateFailed, err: "the stored action is not exactly what its type reads: " + err.Error() + "; nothing ran"}
	} else if refused, err := r.signedApproval(ctx, row, want); err != nil {
		return err // the lease lets the next run check again
	} else if refused != nil && refused.earlier {
		out = outcome{state: StateUncertain, err: refused.msg + "; this attempt did not run, and an earlier one may have acted"}
	} else if refused != nil {
		out = outcome{state: StateFailed, err: refused.msg + "; nothing ran"}
	} else {
		_ = r.event(ctx, row.ID, "claimed", fmt.Sprintf("attempt %d", row.Attempts))
		out = r.run(ctx, row)
	}
	if ctx.Err() != nil {
		return ctx.Err() // stopping; the lease lets the next run observe and finish it
	}
	if out.state == "" {
		if row.Attempts < MaxAttempts {
			_, err := dbq.New(r.Pool).RetryPrivilegedAction(ctx, dbq.RetryPrivilegedActionParams{
				ID: row.ID, Attempts: row.Attempts, LastError: &out.err, DelaySeconds: retryDelay.Seconds(),
			})
			if err == nil {
				err = r.event(ctx, row.ID, "retry", out.err)
			}
			return err
		}
		out.state = StateUncertain
		out.err = "the remote's answer was lost " + fmt.Sprint(MaxAttempts) + " times and the outcome could not be observed: " + out.err
	}
	return r.finish(ctx, row, out)
}

// RemoteTimeout bounds one preflight or one attempt's remote calls, well inside the execution
// lease, so one hung call cannot stall the executor: the attempt retries, and the next observes.
const RemoteTimeout = 90 * time.Second

// run executes one attempt of a claimed action under RemoteTimeout.
func (r *Runner) run(ctx context.Context, row dbq.PrivilegedAction) outcome {
	limit := RemoteTimeout
	switch row.Type {
	case TypeKubectl:
		var a KubectlArgs
		if json.Unmarshal(row.Args, &a) == nil {
			limit += a.timeout() // the dry run again, then the command under its own deadline
		}
	case TypeOmnictl, TypeTalosctl:
		var a SideroArgs
		if json.Unmarshal(row.Args, &a) == nil {
			limit += a.timeout()
		}
	case TypeSSH:
		var a SSHArgs
		if json.Unmarshal(row.Args, &a) == nil {
			limit += a.timeout()
		}
	case TypeUniFi:
		limit += unifiTimeout // the certificate, the object again, then the request
	case TypeExec:
		var a ExecArgs
		if json.Unmarshal(row.Args, &a) == nil {
			limit += a.timeout() + toolboxPendingLimit // the pod's start, then the script under its own deadline
		}
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	switch row.Type {
	case TypePRMerge:
		return r.merge(ctx, row)
	case TypePRClose:
		return r.close(ctx, row)
	case TypeItemToSecret:
		return r.itemToSecret(ctx, row)
	case TypeRolloutRestart, TypeScale:
		return r.workload(ctx, row)
	case TypeDelete:
		return r.deleteObject(ctx, row)
	case TypePatch, TypeApply:
		return r.change(ctx, row)
	case TypeArgoSync:
		return r.argoSync(ctx, row)
	case TypeKubectl:
		return r.kubectl(ctx, row)
	case TypeOmnictl, TypeTalosctl:
		return r.sidero(ctx, row)
	case TypeSSH:
		return r.ssh(ctx, row)
	case TypeUniFi:
		return r.unifi(ctx, row)
	case TypeExec:
		return r.exec(ctx, row)
	default:
		return outcome{state: StateFailed, err: "the type is no longer in the catalog"}
	}
}

func (r *Runner) finish(ctx context.Context, row dbq.PrivilegedAction, out outcome) error {
	var result []byte
	if out.result != nil {
		b, err := json.Marshal(out.result)
		if err != nil {
			return err
		}
		result = b
	}
	var lastErr *string
	if out.err != "" {
		lastErr = &out.err
	}
	return pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		n, err := q.FinishPrivilegedAction(ctx, dbq.FinishPrivilegedActionParams{
			ID: row.ID, Attempts: row.Attempts, State: out.state, Result: result, LastError: lastErr,
		})
		if err != nil || n == 0 {
			return err // n == 0: the lease was lost; the next holder observes
		}
		detail := out.state
		if out.err != "" {
			detail += ": " + out.err
		}
		r.log().InfoContext(ctx, "privileged action finished", "action_id", row.ID, "state", out.state)
		// An uncertain outcome reaches the owner through serve-api's raiseUncertain.
		return q.InsertPrivilegedActionEvent(ctx, dbq.InsertPrivilegedActionEventParams{ActionID: row.ID, Step: "executed", Detail: detail})
	})
}

func (r *Runner) event(ctx context.Context, id, step, detail string) error {
	return dbq.New(r.Pool).InsertPrivilegedActionEvent(ctx, dbq.InsertPrivilegedActionEventParams{ActionID: id, Step: step, Detail: detail})
}

// MergeResult is github.pr_merge's result.
type MergeResult struct {
	MergeSHA string `json:"merge_sha,omitempty"`
	// Observed is set when an earlier attempt had already merged it.
	Observed bool   `json:"observed,omitempty"`
	Comment  string `json:"comment,omitempty"`
}

func (r *Runner) merge(ctx context.Context, row dbq.PrivilegedAction) outcome {
	var f MergeFacts
	if err := json.Unmarshal(row.FrozenFacts, &f); err != nil {
		return outcome{state: StateFailed, err: "the frozen facts cannot be read"}
	}
	repo, err := github.ParseRepo(f.Repo)
	if err != nil {
		return outcome{state: StateFailed, err: "the repository cannot be read"}
	}
	// Observe first: an earlier attempt may have merged it.
	p, err := r.GitHub.PullRequest(ctx, repo, f.PR.Number)
	if err != nil {
		if status, ok := github.StatusOf(err); ok && status == http.StatusNotFound {
			return outcome{state: StateFailed, err: "the pull request can no longer be read"}
		}
		return retry(err)
	}
	switch {
	case p.Merged && p.Head.SHA == f.PR.HeadSHA:
		if row.Attempts == 1 {
			return outcome{state: StateStale, err: "it was already merged by someone else"}
		}
		return outcome{state: StateSucceeded, result: MergeResult{MergeSHA: p.MergeCommitSHA, Observed: true}}
	case p.Merged:
		return outcome{state: StateStale, err: "it was merged at a different head (" + short(p.Head.SHA) + ")"}
	case p.State != "open":
		return outcome{state: StateStale, err: "it was closed"}
	case p.Head.SHA != f.PR.HeadSHA:
		return outcome{state: StateStale, err: "its head moved to " + short(p.Head.SHA) + "; request again so the owner sees the new head"}
	}
	res, err := r.GitHub.MergePullRequest(ctx, repo, f.PR.Number, f.PR.HeadSHA)
	if err != nil {
		status, ok := github.StatusOf(err)
		switch {
		case !ok:
			return retry(err) // unknown whether GitHub merged; the next attempt observes
		case status == http.StatusConflict:
			return outcome{state: StateStale, err: "GitHub refused: the head moved"}
		case status == http.StatusMethodNotAllowed:
			return outcome{state: StateFailed, err: "GitHub refused the merge (status 405): a branch rule, a required check or a conflict blocks it"}
		case status == http.StatusTooManyRequests || status >= 500:
			return retry(err)
		}
		return outcome{state: StateFailed, err: fmt.Sprintf("GitHub refused the merge (status %d)", status)}
	}
	if !res.Merged {
		return outcome{state: StateFailed, err: "GitHub answered that it did not merge"}
	}
	out := MergeResult{MergeSHA: res.SHA}
	body := fmt.Sprintf("Merged by Mainloop at %s after the owner approved privileged action `%s` (digest `%s`).",
		short(f.PR.HeadSHA), row.ID, (*row.Digest)[:16])
	if c, err := r.GitHub.CommentIssue(ctx, repo, f.PR.Number, body); err == nil {
		out.Comment = c.HTMLURL
	} else {
		_ = r.event(ctx, row.ID, "comment_failed", safeError(err))
	}
	return outcome{state: StateSucceeded, result: out}
}

// CloseResult is github.pr_close's result: what happened to each pull request.
type CloseResult struct {
	PRs []ClosedPR `json:"prs"`
}

// ClosedPR is one pull request's result: closed, already_closed, merged, stale or failed.
type ClosedPR struct {
	Number int    `json:"number"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
}

func (r *Runner) close(ctx context.Context, row dbq.PrivilegedAction) outcome {
	var f CloseFacts
	var a CloseArgs
	if json.Unmarshal(row.FrozenFacts, &f) != nil || json.Unmarshal(row.Args, &a) != nil {
		return outcome{state: StateFailed, err: "the frozen facts cannot be read"}
	}
	repo, err := github.ParseRepo(f.Repo)
	if err != nil {
		return outcome{state: StateFailed, err: "the repository cannot be read"}
	}
	res := CloseResult{}
	all := true
	for _, want := range f.PRs {
		p, err := r.GitHub.PullRequest(ctx, repo, want.Number)
		if err != nil {
			if status, ok := github.StatusOf(err); ok && status == http.StatusNotFound {
				res.PRs = append(res.PRs, ClosedPR{Number: want.Number, Result: "failed", Detail: "no longer readable"})
				all = false
				continue
			}
			return retry(err)
		}
		switch {
		case p.Merged:
			res.PRs = append(res.PRs, ClosedPR{Number: want.Number, Result: "merged", Detail: "merged meanwhile; left as it is"})
			all = false
			continue
		case p.State != "open":
			res.PRs = append(res.PRs, ClosedPR{Number: want.Number, Result: "already_closed"})
			continue
		case p.Head.SHA != want.HeadSHA:
			res.PRs = append(res.PRs, ClosedPR{Number: want.Number, Result: "stale", Detail: "head moved to " + short(p.Head.SHA) + "; left open"})
			all = false
			continue
		}
		if err := r.GitHub.ClosePullRequest(ctx, repo, want.Number); err != nil {
			status, ok := github.StatusOf(err)
			if !ok || status == http.StatusTooManyRequests || status >= 500 {
				return retry(err) // the next attempt observes which ones closed
			}
			res.PRs = append(res.PRs, ClosedPR{Number: want.Number, Result: "failed", Detail: fmt.Sprintf("GitHub refused (status %d)", status)})
			all = false
			continue
		}
		res.PRs = append(res.PRs, ClosedPR{Number: want.Number, Result: "closed"})
		body := fmt.Sprintf("Closed by Mainloop after the owner approved privileged action `%s`.", row.ID)
		if a.Comment != "" {
			body = a.Comment + "\n\n" + body
		}
		if _, err := r.GitHub.CommentIssue(ctx, repo, want.Number, body); err != nil {
			_ = r.event(ctx, row.ID, "comment_failed", fmt.Sprintf("#%d: %s", want.Number, safeError(err)))
		}
	}
	if all {
		return outcome{state: StateSucceeded, result: res}
	}
	return outcome{state: StateFailed, result: res, err: "not every pull request was closed; see the result"}
}

// deliver sends the outcome to the requesting session once; the action id is the message id.
func (r *Runner) deliver(ctx context.Context, row dbq.PrivilegedAction) error {
	settle := func(state, detail string, delay time.Duration) error {
		var d *string
		if detail != "" {
			d = &detail
		}
		_, err := dbq.New(r.Pool).SettlePrivilegedDelivery(ctx, dbq.SettlePrivilegedDeliveryParams{
			ID: row.ID, DeliveryAttempts: row.DeliveryAttempts, DeliveryState: state, DeliveryDetail: d,
			DelaySeconds: delay.Seconds(),
		})
		if err == nil && state != DeliveryPending {
			err = r.event(ctx, row.ID, "delivered", state+" "+detail)
		}
		return err
	}
	if r.Sender == nil {
		return settle(DeliveryPending, "no sender in this process", time.Minute)
	}
	_, err := r.Sender.SendToWith(ctx, sessions.SourceMainloop, row.RequesterSessionID, row.ID, Message(row), sessions.SendOptions{Steer: true})
	switch {
	case err == nil:
		return settle(DeliveryDelivered, "", 0)
	case errors.Is(err, sessions.ErrNotFound):
		return settle(DeliveryUndeliverable, "the session is gone", 0)
	case row.DeliveryAttempts >= MaxDeliveryAttempts:
		return settle(DeliveryUndeliverable, safeError(err), 0)
	case errors.Is(err, sessions.ErrBusy):
		return settle(DeliveryPending, "", 3*time.Second)
	}
	return settle(DeliveryPending, safeError(err), 30*time.Second)
}

// Message is the one message an action's requesting session gets when it ends.
func Message(row dbq.PrivilegedAction) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Privileged action %s (%s, request %s) ended: %s.", row.ID, row.Type, row.RequestID, row.State)
	switch row.State {
	case StateDenied:
		if row.DecisionNote != nil {
			b.WriteString(" The owner's note: " + *row.DecisionNote)
		} else {
			b.WriteString(" The owner denied it without a note.")
		}
	case StateExpired:
		b.WriteString(" Nobody decided (or nothing ran) in time; nothing changed.")
	case StateStale:
		if len(row.Result) > 0 {
			b.WriteString(" The target changed since the owner saw it, so it stopped; an earlier attempt had already written what the result lists. Request again if the rest is still needed.")
		} else {
			b.WriteString(" The target changed since the owner saw it, so nothing ran; request again if it is still needed.")
		}
	case StateUncertain:
		b.WriteString(" Mainloop could not tell whether it ran; the owner has an item. Check the target before requesting again.")
	}
	if row.LastError != nil && row.State != StateSucceeded {
		b.WriteString(" Detail: " + *row.LastError)
	}
	if len(row.Result) > 0 {
		b.WriteString(" Result: " + string(row.Result))
	}
	b.WriteString(" get_privileged_action gives the full record.")
	return b.String()
}
