package tasks

import (
	"context"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// StopObserver is the resolver of an unconfirmed stop. It reads kagent for the session of an attempt
// whose cancel kagent did not confirm, and asks again for what still runs.
type StopObserver interface {
	// Stopped reports whether nothing runs for the session any more. When something still does, it
	// asks kagent to cancel it and reports false; the next call reads the result.
	Stopped(ctx context.Context, sessionID string) (bool, error)
}

// kagentTasks is the part of the kagent client the observer uses.
type kagentTasks interface {
	GetTask(ctx context.Context, agent kagent.AgentRef, taskID string) (*a2a.Task, error)
	FindTaskForMessage(ctx context.Context, agent kagent.AgentRef, contextID, messageID string) (*a2a.Task, bool, error)
	CancelTask(ctx context.Context, agent kagent.AgentRef, taskID string) (*a2a.Task, error)
}

// KagentStops observes the session ledger and kagent. It looks at the most recent deliveries of the
// session: every task kagent started for one is read, and one that is not in a final state is
// cancelled again.
type KagentStops struct {
	Pool    *pgxpool.Pool
	OwnerID string
	// Sessions resolves the Agent that runs a session.
	Sessions *sessions.Service
	Kagent   kagentTasks
}

// recentDeliveries is how many deliveries of a session the observer reads.
const recentDeliveries = 20

// Stopped implements [StopObserver].
func (k *KagentStops) Stopped(ctx context.Context, sessionID string) (bool, error) {
	q := dbq.New(k.Pool)
	b, err := q.GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: k.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // no binding, no agent
	}
	if err != nil {
		return false, err
	}
	if b.KagentSessionID == nil || *b.KagentSessionID == "" {
		return true, nil // kagent never got a session for it
	}
	agent, err := k.Sessions.ResolveAgent(b.WorkspaceAgentNamespace, b.WorkspaceAgentName, &b.Kind, b.Role)
	if err != nil {
		return false, err
	}
	rows, err := q.ListSessionDeliveries(ctx, dbq.ListSessionDeliveriesParams{SessionID: sessionID, Limit: recentDeliveries})
	if err != nil {
		return false, err
	}
	stopped := true
	for _, d := range rows {
		ksid := *b.KagentSessionID
		if d.KagentSessionID != nil && *d.KagentSessionID != "" {
			ksid = *d.KagentSessionID
		}
		task, err := k.taskOf(ctx, agent, ksid, d)
		if err != nil {
			return false, err
		}
		if task == nil || task.Status.State.Terminal() {
			continue
		}
		stopped = false
		if _, err := k.Kagent.CancelTask(ctx, agent, string(task.ID)); err != nil {
			var gone *kagent.TaskNotFound
			if !errors.As(err, &gone) {
				return false, fmt.Errorf("cancel task: %w", err)
			}
		}
	}
	return stopped, nil
}

// taskOf is the kagent task of a delivery: the one it recorded, else the one that carries its
// message. A task kagent no longer has, or a message it never saw, gives nil.
func (k *KagentStops) taskOf(ctx context.Context, agent kagent.AgentRef, ksid string, d dbq.Delivery) (*a2a.Task, error) {
	if d.TaskID != nil && *d.TaskID != "" {
		task, err := k.Kagent.GetTask(ctx, agent, *d.TaskID)
		var gone *kagent.TaskNotFound
		if errors.As(err, &gone) {
			return nil, nil
		}
		return task, err
	}
	task, found, err := k.Kagent.FindTaskForMessage(ctx, agent, ksid, d.ID)
	if err != nil && !found {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return task, nil
}
