package tasks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// StopObserver confirms that the runtime of a stopping attempt has stopped. It is the only thing
// that does: the result of cancelling a session never confirms a stop.
type StopObserver interface {
	// Stopped reports whether nothing runs for the session any more. When something still does, it
	// asks kagent to cancel it and reports false; the next call reads the result. An error means the
	// session could not be observed, which is not a stop.
	Stopped(ctx context.Context, sessionID string) (bool, error)
}

// KagentTasks is the part of the kagent client the observer uses. *kagent.Client satisfies it.
type KagentTasks interface {
	ListTasks(ctx context.Context, agent kagent.AgentRef, contextID string) ([]*a2a.Task, error)
	FindTaskForMessage(ctx context.Context, agent kagent.AgentRef, contextID, messageID string) (*a2a.Task, bool, error)
	CancelTask(ctx context.Context, agent kagent.AgentRef, taskID string) (*a2a.Task, error)
}

// KagentSessionLiveness is the part of the kagent client that tells whether a resolved kagent
// session still exists at all. *kagent.Client satisfies it through ObserveLiveness.
type KagentSessionLiveness interface {
	ObserveLiveness(ctx context.Context, kagentSessionID string) (kagent.SessionLiveness, error)
}

// KagentStops observes a session in kagent. It proves the session, not its deliveries: every native
// context the session has used is listed, and any task in it that is not final is cancelled.
type KagentStops struct {
	Pool    *pgxpool.Pool
	OwnerID string
	// Sessions resolves the Agent that runs a session.
	Sessions *sessions.Service
	Kagent   KagentTasks
	// Liveness, when set, is checked before anything else: a session kagent has entirely forgotten
	// (for instance across a kagent restart with no persistence), or that has settled into a
	// terminal runtime state it will never leave (failed or deleted), answers stopped at once, so a
	// cancel of an attempt whose session is gone settles instead of retrying
	// ListTasks/FindTaskForMessage forever against a session that will never reappear. A session
	// still on its way out (deleting) is not shortcut this way: it may still carry a task kagent can
	// list, and that listing is what cancels it, so it goes through the normal path below. Nil skips
	// the check entirely.
	Liveness KagentSessionLiveness
	// UncertainHold is how long a message that was sent, and that no task carries yet, may still
	// land. It is the sessions ledger's hold; zero takes that service's setting, then 30 minutes.
	UncertainHold time.Duration
}

// defaultUncertainHold is the sessions ledger's default hold.
const defaultUncertainHold = 30 * time.Minute

func (k *KagentStops) hold() time.Duration {
	switch {
	case k.UncertainHold > 0:
		return k.UncertainHold
	case k.Sessions != nil && k.Sessions.Opts.UncertainHold > 0:
		return k.Sessions.Opts.UncertainHold
	}
	return defaultUncertainHold
}

// Stopped implements [StopObserver]. With Liveness set, it is stopped at once when kagent has no
// record of the session at all or reports it settled into a terminal runtime state (failed or
// deleted): no native context can still carry a live task for a session kagent has forgotten or
// finished tearing down. A session still deleting is not shortcut: it goes through the normal path
// below, since it may still list a task that needs cancelling. Otherwise it is stopped when
//
//   - every native context it has used (the binding's kagent session, and the one each delivery was
//     sent to, all of them) lists only final tasks, however they were started and whether or not a
//     delivery row knows them; any task that is not final is cancelled, which is safe to repeat, and
//     the session is not stopped yet;
//   - no listing or cancel failed: a read error is never a stop; and
//   - no message that was sent and that no task carries is still inside the hold, because it may yet
//     land and start a task. This covers a send that was in flight, and one a cancel closed. A message
//     kagent shows a task for is judged by that task, and one it shows but cannot read is an error.
func (k *KagentStops) Stopped(ctx context.Context, sessionID string) (bool, error) {
	q := dbq.New(k.Pool)
	b, err := q.GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: k.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // no binding, no agent
	}
	if err != nil {
		return false, err
	}
	if k.Liveness != nil && b.KagentSessionID != nil && *b.KagentSessionID != "" {
		live, err := k.Liveness.ObserveLiveness(ctx, *b.KagentSessionID)
		if err != nil {
			return false, err
		}
		if !live.Exists || (live.Terminal && !live.Deleting) {
			return true, nil // the session is gone or has settled terminal: nothing in it is still running
		}
	}
	contexts, err := q.ListSessionNativeContexts(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if b.KagentSessionID != nil && *b.KagentSessionID != "" {
		contexts = append(contexts, *b.KagentSessionID)
	}
	slices.Sort(contexts)
	contexts = slices.Compact(contexts)
	pending, err := q.ListSessionPossiblyLandingDeliveries(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if len(contexts) == 0 {
		return true, nil // nothing was ever sent
	}
	if k.Kagent == nil {
		return false, errors.New("tasks: the kagent client cannot list tasks, so a stop cannot be observed")
	}
	agent, err := k.Sessions.ResolveAgent(&b.ResolvedAgentNamespace, &b.ResolvedAgentName, &b.Kind, b.Role)
	if err != nil {
		return false, err
	}
	stopped := true
	cancelled := map[a2a.TaskID]bool{}
	cancel := func(t *a2a.Task) error {
		stopped = false
		if cancelled[t.ID] {
			return nil
		}
		cancelled[t.ID] = true
		if _, err := k.Kagent.CancelTask(ctx, agent, string(t.ID)); err != nil {
			var gone *kagent.TaskNotFound
			if !errors.As(err, &gone) {
				return fmt.Errorf("cancel task: %w", err)
			}
		}
		return nil
	}
	for _, c := range contexts {
		listed, err := k.Kagent.ListTasks(ctx, agent, c)
		if err != nil {
			return false, fmt.Errorf("list tasks: %w", err)
		}
		for _, t := range listed {
			if t != nil && !t.Status.State.Terminal() {
				if err := cancel(t); err != nil {
					return false, err
				}
			}
		}
	}
	for _, d := range pending {
		task, found, err := k.Kagent.FindTaskForMessage(ctx, agent, *d.KagentSessionID, d.ID)
		if err != nil {
			// Found-but-unreadable is an error too: the message exists, so its task is unobserved.
			return false, fmt.Errorf("find task for message: %w", err)
		}
		switch {
		case found && task != nil:
			if !task.Status.State.Terminal() {
				if err := cancel(task); err != nil {
					return false, err
				}
			}
		case found:
			return false, errors.New("find task for message: found without a task")
		case time.Since(d.UpdatedAt) <= k.hold():
			// No task carries it yet. For a row a cancel closed, UpdatedAt is the cancel time: a message
			// still in flight can only land after it.
			stopped = false
		}
	}
	return stopped, nil
}
