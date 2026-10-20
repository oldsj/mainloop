package tasks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/sessions"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
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
	// Workspaces and Deleter, when set with Liveness, let [KagentStops.ForceStop] end a runtime that
	// does not answer a cancel: Workspaces suspends it, Deleter deletes it. Nil leaves that step to
	// the ordinary observation.
	Workspaces WorkspaceSuspender
	Deleter    KagentSessionDeleter
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
// record of the session at all, reports it settled into a terminal runtime state (failed or
// deleted), or reports it suspended: no native context can still carry a live task for a session
// kagent has forgotten, finished tearing down or parked. A session still deleting is not shortcut: it goes through the normal path
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
		if runtimeStopped(live) {
			return true, nil // gone, settled terminal or suspended: nothing in it is still running
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

// ForceLevel is how hard a stop that cancelling did not settle may be pressed.
type ForceLevel int

const (
	// ForceNone leaves the stop to the ordinary observation.
	ForceNone ForceLevel = iota
	// ForceSuspend parks the runtime through the workspace service. The workspace and any unpushed
	// work in it survive, and the turn cannot continue.
	ForceSuspend
	// ForceDelete has kagent delete the native session. The runtime and its workspace are gone, but
	// Mainloop's rows are kept as audit history.
	ForceDelete
)

func (l ForceLevel) String() string {
	switch l {
	case ForceSuspend:
		return "suspend"
	case ForceDelete:
		return "delete"
	}
	return "none"
}

// StopForcer is implemented by an observer that can also end a session's runtime itself. The resolver
// asks it once cancelling has failed to settle a stop for long enough: a runtime that is crashed,
// wedged or never answers a cancel would otherwise hold the attempt's claim and capacity for good.
type StopForcer interface {
	// ForceStop reports whether the session's runtime has stopped. When it has not, it presses the
	// stop as hard as level allows and reports whether that settled it. An error means the session
	// could not be observed or pressed, which is not a stop.
	ForceStop(ctx context.Context, sessionID string, level ForceLevel) (bool, error)
}

// KagentSessionDeleter is the part of the kagent client that deletes a session. *kagent.Client
// satisfies it.
type KagentSessionDeleter interface {
	DeleteSession(ctx context.Context, id string) (*kagentapi.Session, error)
}

// WorkspaceSuspender is the part of [workspaces.Service] a forced stop uses: its suspend is a durable
// operation that refuses while a turn is open and waits for kagent to confirm.
type WorkspaceSuspender interface {
	Suspend(ctx context.Context, id string) (api.WorkspaceLifecycle, error)
}

// ForceStop implements [StopForcer]. It checks before it acts, so a repeat never presses twice: a
// session already gone, settled terminal or suspended is stopped (nothing in it runs until something
// resumes it, and a stopped attempt takes no more sends); one still on its way out (deleting) is
// waited for.
//
// ForceSuspend suspends through the workspace service, which keeps the workspace and its unpushed
// work. It refuses while the ledger still holds a turn open, and then the stop waits for
// ForceDelete. ForceDelete asks kagent to delete the native session directly rather than through the
// workspace service, whose delete purges the session, binding, deliveries and approvals rows: an
// attempt is audit history. The attempt's native_deleted_at and the binding's kagent_deleted_at record
// the delete as soon as kagent accepts it, so nothing resumes or reuses the session. kagent refuses a
// delete with Aborted while another lifecycle operation runs; that is an error and the next look
// repeats it. Without a Liveness it does nothing and reports false.
func (k *KagentStops) ForceStop(ctx context.Context, sessionID string, level ForceLevel) (bool, error) {
	if k.Liveness == nil || level == ForceNone {
		return false, nil
	}
	q := dbq.New(k.Pool)
	b, err := q.GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: k.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // no binding, no agent
	}
	if err != nil {
		return false, err
	}
	if b.KagentSessionID == nil || *b.KagentSessionID == "" {
		return false, nil // no native session to end; the ordinary observation decides
	}
	live, err := k.Liveness.ObserveLiveness(ctx, *b.KagentSessionID)
	if err != nil {
		return false, err
	}
	switch {
	case runtimeStopped(live):
		return true, nil
	case live.Deleting:
		return false, nil // already on its way out; read the result on the next look
	}
	if level == ForceSuspend {
		if k.Workspaces == nil {
			return false, nil
		}
		if _, err := k.Workspaces.Suspend(ctx, sessionID); err != nil {
			return false, fmt.Errorf("suspend workspace: %w", err)
		}
		live, err = k.Liveness.ObserveLiveness(ctx, *b.KagentSessionID)
		return err == nil && runtimeStopped(live), err
	}
	if k.Deleter == nil {
		return false, nil
	}
	if _, err := k.Deleter.DeleteSession(ctx, *b.KagentSessionID); err != nil {
		var se *kagent.SessionError
		if !errors.As(err, &se) || !se.Wire || se.Code != connect.CodeNotFound {
			return false, fmt.Errorf("delete session: %w", err)
		}
	}
	if err := q.MarkAttemptNativeDeleted(ctx, &sessionID); err != nil {
		return false, err
	}
	if err := q.MarkBindingKagentDeleted(ctx, sessionID); err != nil {
		return false, err
	}
	live, err = k.Liveness.ObserveLiveness(ctx, *b.KagentSessionID)
	return err == nil && runtimeStopped(live), err
}

// runtimeStopped reports whether nothing can run in a session of this liveness: kagent has no record
// of it, it settled failed or deleted, or it is suspended. A session still deleting is not stopped
// yet; it may list a task that needs cancelling.
func runtimeStopped(live kagent.SessionLiveness) bool {
	return !live.Exists || (live.Terminal && !live.Deleting) || live.Suspended
}
