package tasks

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// Defaults for the liveness sweep's bounds, taken when configuration gives none: 45 minutes of no
// activity, 6 hours of total runtime, and a minute between passes. They are a safety net, not the
// usual outcome of a task.
const (
	defaultStallTimeout  = 45 * time.Minute
	defaultMaxRuntime    = 6 * time.Hour
	defaultLivenessEvery = time.Minute
)

// LivenessObserver reads whether an attempt's kagent session still exists, whether its runtime can
// no longer run again, and when it last showed activity, resolved from sessionID — the Mainloop
// session/binding id an attempt's binding_id names, the same id [StopObserver.Stopped] takes, never
// a kagent session id. known is false when no kagent session has been created for it yet (a
// coordination attempt before its brief is delivered); the caller must leave the attempt alone,
// never treat that as lost. [KagentLiveness] is the production implementation.
type LivenessObserver interface {
	ObserveLiveness(ctx context.Context, sessionID string) (live kagent.SessionLiveness, known bool, err error)
}

// KagentLivenessClient is the part of the kagent client [KagentLiveness] uses: whether a resolved
// kagent session still exists and is live, and the A2A tasks of one of its contexts, each with at
// most its newest history message — status and that message are all that is judged here. *kagent.Client
// satisfies it.
type KagentLivenessClient interface {
	ObserveLiveness(ctx context.Context, kagentSessionID string) (kagent.SessionLiveness, error)
	ListTasksLatest(ctx context.Context, agent kagent.AgentRef, contextID string) ([]*a2a.Task, error)
}

// ActivityReader is the part of the sessions delivery ledger [KagentLiveness] uses for the fallback
// activity signal when kagent shows no A2A task at all for a session. *sessions.Ledger satisfies it.
type ActivityReader interface {
	LastActivity(ctx context.Context, sessionID string) (time.Time, error)
}

// KagentLiveness observes an attempt's kagent session the way [KagentStops] observes a stop: from
// sessionID it resolves the binding's kagent_session_id fresh from the database on every call,
// outside any transaction, and only then asks kagent — Mainloop's own session id is never a kagent
// id. Existence and terminality come from the Session itself. Activity does not: kagent's own
// Session.updated_at only moves on a lifecycle operation (create, rename, resume, suspend, delete),
// never on a turn, so a long working turn would otherwise look idle. Activity instead comes from
// the A2A tasks of every native context the session has used (the same contexts
// [KagentStops.Stopped] lists): one waiting on the owner (input-required or auth-required) means
// activity right now — the agent is not stalled while it waits for a reply, though the runtime bound
// still applies. A submitted or working task does not: kagent reports a turn as working until it
// ends, and a turn whose native process wedged never ends, so working is not proof of life. It is
// fingerprinted instead ([taskProgress]) and the sweep counts activity from when it last saw the
// fingerprint change. Otherwise the later of the newest task status timestamp and the delivery
// ledger's own last-activity record stands in.
type KagentLiveness struct {
	Pool    *pgxpool.Pool
	OwnerID string
	// Sessions resolves the Agent that runs a session.
	Sessions *sessions.Service
	Kagent   KagentLivenessClient
	// Activity is consulted when kagent shows no A2A task for the session at all. *sessions.Ledger
	// satisfies it; nil skips it (no fallback, only what kagent itself reports).
	Activity ActivityReader
	// Now is the clock for activity timestamps; time.Now when nil.
	Now func() time.Time
}

func (k *KagentLiveness) now() time.Time {
	if k.Now != nil {
		return k.Now().UTC()
	}
	return time.Now().UTC()
}

// ObserveLiveness implements [LivenessObserver].
func (k *KagentLiveness) ObserveLiveness(ctx context.Context, sessionID string) (kagent.SessionLiveness, bool, error) {
	q := dbq.New(k.Pool)
	b, err := q.GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: k.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return kagent.SessionLiveness{}, false, nil
	}
	if err != nil {
		return kagent.SessionLiveness{}, false, err
	}
	if b.KagentSessionID == nil || *b.KagentSessionID == "" {
		return kagent.SessionLiveness{}, false, nil // no kagent session yet
	}
	ksid := *b.KagentSessionID
	live, err := k.Kagent.ObserveLiveness(ctx, ksid)
	if err != nil {
		return kagent.SessionLiveness{}, true, err
	}
	if !live.Exists || live.Terminal {
		return live, true, nil // gone or done: no point reading its activity
	}
	activity, progress, err := k.activity(ctx, q, sessionID, b, ksid)
	if err != nil {
		return kagent.SessionLiveness{}, true, err
	}
	live.LastActivity, live.Progress = activity, progress
	return live, true, nil
}

// activity reads what kagent shows of the session's turns: LastActivity is the later of now, if
// any native context lists a task waiting on the owner (input-required or auth-required — the
// agent is blocked on a reply, not idle), and otherwise the newest status timestamp of any task
// any context listed or the delivery ledger's own record. Progress is a fingerprint of every
// submitted or working task: kagent keeps such a task "working" for as long as its turn has not
// ended, including a turn whose native process wedged, so working itself is no evidence of
// activity — only a change in the fingerprint is, and [Service.checkLiveness] times that.
func (k *KagentLiveness) activity(
	ctx context.Context, q *dbq.Queries, sessionID string, b dbq.GetOwnedSessionBindingRow, ksid string,
) (last time.Time, progress string, err error) {
	contexts, err := q.ListSessionNativeContexts(ctx, sessionID)
	if err != nil {
		return time.Time{}, "", err
	}
	contexts = append(contexts, ksid)
	slices.Sort(contexts)
	contexts = slices.Compact(contexts)
	agent, err := k.Sessions.ResolveAgent(&b.ResolvedAgentNamespace, &b.ResolvedAgentName, &b.Kind, b.Role)
	if err != nil {
		return time.Time{}, "", err
	}
	var latest time.Time
	var running []string
	for _, c := range contexts {
		listed, err := k.Kagent.ListTasksLatest(ctx, agent, c)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("list tasks: %w", err)
		}
		for _, t := range listed {
			if t == nil {
				continue
			}
			switch t.Status.State {
			case a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired:
				return k.now(), "", nil
			case a2a.TaskStateSubmitted, a2a.TaskStateWorking:
				running = append(running, taskProgress(t))
			}
			if ts := t.Status.Timestamp; ts != nil && ts.After(latest) {
				latest = *ts
			}
		}
	}
	slices.Sort(running)
	progress = strings.Join(running, ";")
	if k.Activity == nil {
		return latest, progress, nil
	}
	fromLedger, err := k.Activity.LastActivity(ctx, sessionID)
	if err != nil {
		return time.Time{}, "", err
	}
	return laterOf(latest, fromLedger), progress, nil
}

// taskProgress fingerprints a running task by what kagent has recorded for it: its id, state and
// status time, and the id of its newest history message. Every tool call and result of a live turn
// adds a message, so a turn that is doing anything changes this, and one that is not leaves it as
// it was.
func taskProgress(t *a2a.Task) string {
	newest := ""
	if n := len(t.History); n > 0 && t.History[n-1] != nil {
		newest = t.History[n-1].ID
	}
	stamp := ""
	if t.Status.Timestamp != nil {
		stamp = t.Status.Timestamp.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%s|%s|%s|%s", t.ID, t.Status.State, stamp, newest)
}

// lastActivity is when the attempt last showed activity: the later of what the observation itself
// reports and, for a running turn, when the sweep first saw its current fingerprint. changed says
// that fingerprint is not the one stored for the attempt, so the caller records it. A turn with
// no fingerprint (nothing is running) has nothing to time.
func lastActivity(live kagent.SessionLiveness, a dbq.TaskAttempt, now time.Time) (at time.Time, changed bool) {
	if live.Progress == "" {
		return live.LastActivity, false
	}
	seen := now
	if a.ProgressMarker != nil && *a.ProgressMarker == live.Progress && a.ProgressAt != nil {
		seen = *a.ProgressAt
	} else {
		changed = true
	}
	return laterOf(live.LastActivity, seen), changed
}

func (s *Service) stallTimeout() time.Duration { return cmp.Or(s.Limits.StallTimeout, defaultStallTimeout) }
func (s *Service) maxRuntime() time.Duration    { return cmp.Or(s.Limits.MaxRuntime, defaultMaxRuntime) }
func (s *Service) livenessEvery() time.Duration { return cmp.Or(s.Limits.LivenessEvery, defaultLivenessEvery) }

// LivenessSweep bounds every task whose current attempt is active: it is blocked, once, when
// kagent reports the attempt's session lost or terminal (session_lost), when it has shown no
// activity for the stall timeout (stalled), or when the attempt has run longer than the runtime
// bound (runtime_limit; a turn kagent keeps recording progress for counts as activity for the
// stall clock but never exempts an attempt from this bound; a turn kagent still calls working but
// records nothing new for does not). It cancels nothing native and retries nothing; it only raises the
// owner's attention, through the task's reason, so a session kagent has forgotten about does not
// leave a task reading "running" forever. A progress report from the task's current attempt
// recovers session_lost and stalled back to running — the sweep can be wrong, and a report is
// proof it was; runtime_limit stays blocked, since it fires regardless of activity and recovering
// it on its own say-so would defeat the bound.
func (s *Service) LivenessSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "task-liveness", Every: s.livenessEvery(), Run: s.runLiveness}
}

func (s *Service) runLiveness(ctx context.Context) error {
	if s.Liveness == nil {
		return nil
	}
	q := dbq.New(s.Pool)
	rows, err := q.ListActiveAttemptsForLiveness(ctx, 100)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range rows {
		if err := s.checkLiveness(ctx, a); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// checkLiveness looks at one active attempt and blocks its task on the first bound it crosses. A
// session kagent has lost or ended is checked first: there is no point timing anything out once
// the session itself is gone. The stall timeout is checked next, since it is normally far shorter
// than the runtime bound and is the more informative reason; the runtime bound is the last-resort
// safety net, for a session that keeps reporting activity without ever finishing. Every path that
// does not block touches the attempt, so the next sweep's query (ordered by updated_at) rotates
// through attempts beyond one pass's limit instead of always picking the same oldest rows.
func (s *Service) checkLiveness(ctx context.Context, a dbq.TaskAttempt) error {
	if a.BindingID == nil {
		return nil
	}
	live, known, err := s.Liveness.ObserveLiveness(ctx, *a.BindingID)
	if err != nil {
		s.log().WarnContext(ctx, "tasks: observe attempt liveness", "attempt", a.ID, "error", err)
		return s.touchLiveness(ctx, a.ID)
	}
	if !known {
		return s.touchLiveness(ctx, a.ID) // no kagent session yet; nothing to judge
	}
	now := s.now()
	active, changed := lastActivity(live, a, now)
	if changed && live.Exists && !live.Terminal {
		if err := dbq.New(s.Pool).RecordAttemptProgress(ctx, dbq.RecordAttemptProgressParams{
			ID: a.ID, ProgressMarker: &live.Progress, ProgressAt: &now,
		}); err != nil {
			return err
		}
	}
	switch {
	case !live.Exists || live.Terminal:
		return s.blockAttempt(ctx, a, dbstate.ReasonSessionLost)
	case !active.IsZero() && now.Sub(active) > s.stallTimeout():
		return s.blockAttempt(ctx, a, dbstate.ReasonStalled)
	case now.Sub(a.CreatedAt) > s.maxRuntime():
		return s.blockAttempt(ctx, a, dbstate.ReasonRuntimeLimit)
	}
	return s.touchLiveness(ctx, a.ID)
}

// touchLiveness bumps the attempt's updated_at alone; see [Service.checkLiveness].
func (s *Service) touchLiveness(ctx context.Context, attemptID string) error {
	return dbq.New(s.Pool).TouchAttemptLiveness(ctx, attemptID)
}

// blockAttempt moves an attempt's task to blocked under reason, once. The attempt must still be
// active and the task's current one, and — since the observation above took no lock and the task
// may have moved on while it was in flight — the task must still be exactly running with no reason:
// anything else (a report that completed it, blocked it on an owner question, or anything else)
// owns the task now, and the liveness signal that raced it is stale. The task row is locked first,
// then the attempt is re-read under that lock, the same order [Service.drain] uses: a write between
// the sweep's read and this one (settling, fencing, replacing) is seen before this decides anything.
func (s *Service) blockAttempt(ctx context.Context, a dbq.TaskAttempt, reason dbstate.TaskReason) error {
	return s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		t, err := q.GetTaskByIDForUpdate(ctx, a.TaskID)
		if err != nil {
			return nil, err
		}
		cur, err := q.GetTaskAttempt(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		if cur.State != dbstate.AttemptActive {
			return nil, nil // drained, fenced or replaced since the sweep read it
		}
		if t.CurrentAttemptID == nil || *t.CurrentAttemptID != a.ID || t.Status != dbstate.TaskRunning || t.Reason != nil {
			return nil, nil
		}
		to := TaskState{Status: dbstate.TaskBlocked, Reason: reason}
		if err := TransitionTask(TaskState{Status: dbstate.TaskRunning}, to); err != nil {
			return nil, nil
		}
		after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: to.Status, Reason: &reason})
		if err != nil {
			return nil, err
		}
		return []events.Draft{s.updated(after)}, nil
	})
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
