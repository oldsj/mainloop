package tasks

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// Principal is who is acting on tasks: the owner through the API, the main thread, or the
// supervisor or child of a task attempt. The task fields are set only for the last two, and only
// by [Service.Resolve], which checks that the attempt is still the live current one.
type Principal struct {
	OwnerID string
	// Role is empty for the owner's own requests.
	Role      dbstate.BindingRole
	BindingID string
	TaskID    string
	AttemptID string
	// Final is set when the principal's task is completed and its attempt is the completed
	// current one. Such a principal can still file the one completed report, and nothing else.
	Final bool
}

// OwnerPrincipal is the principal of an owner request.
func OwnerPrincipal(owner string) Principal { return Principal{OwnerID: owner} }

// Key is the principal part of an operation's idempotency key.
func (p Principal) Key() string {
	if p.BindingID == "" {
		return "owner:" + p.OwnerID
	}
	return "binding:" + p.BindingID
}

func (p Principal) isTaskAgent() bool {
	return p.Role == dbstate.RoleSupervisor || p.Role == dbstate.RoleChild
}

// canManage reports whether the principal may create and change tasks at all.
func (p Principal) canManage() bool {
	return p.Role == "" || p.Role == dbstate.RoleMain || p.Role == dbstate.RoleSupervisor
}

// access returns what the principal may do with a task it can name. The owner and the main thread
// see and manage every task. A supervisor sees its own task, which it cannot manage, and manages
// its children. A child sees only its own task.
func (p Principal) access(t dbq.Task) (read, manage bool) {
	if p.Final && t.ID != p.TaskID {
		// The attempt of a completed task looks at its own task and nothing else.
		return false, false
	}
	switch p.Role {
	case "", dbstate.RoleMain:
		return true, true
	case dbstate.RoleSupervisor:
		if t.ID == p.TaskID {
			return true, false
		}
		own := t.ParentTaskID != nil && *t.ParentTaskID == p.TaskID
		return own, own
	case dbstate.RoleChild:
		return t.ID == p.TaskID, false
	}
	return false, false
}

// AuthState is what the MCP listener needs to decide an agent's depth.
type AuthState struct {
	Depth int
	// MergeStatusOnly marks the attempt of a completed task.
	MergeStatusOnly bool
}

// ErrInactive means a supervisor or child binding does not name the live current attempt of its
// task. The MCP authenticator turns it into an unauthorized answer.
var ErrInactive = errors.New("tasks: binding is not a live current task attempt")

// principalOf loads the attempt behind a supervisor or child binding and checks it: the attempt
// must be the task's current one, and live; or, for a completed task, the completed current one.
func principalOf(ctx context.Context, q *dbq.Queries, role dbstate.BindingRole, binding string) (dbq.GetAttemptPrincipalRow, bool, error) {
	row, err := q.GetAttemptPrincipal(ctx, &binding)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, false, ErrInactive
	}
	if err != nil {
		return row, false, err
	}
	want := dbstate.AttemptSupervisor
	if role == dbstate.RoleChild {
		want = dbstate.AttemptChild
	}
	if row.AttemptRole != want || !row.IsCurrent {
		return row, false, ErrInactive
	}
	switch {
	case AttemptLive(row.AttemptState) && !TaskFinal(row.TaskStatus):
		return row, false, nil
	case row.AttemptState == dbstate.AttemptCompleted && row.TaskStatus == dbstate.TaskCompleted:
		return row, true, nil
	}
	return row, false, ErrInactive
}

// Auth resolves the depth of a supervisor or child binding for the MCP authenticator. mergeTools
// is whether the merge tools are on: only then, and only for a workspace grant, may the attempt of
// a completed task still look at merge status.
func (s *Service) Auth(ctx context.Context, role dbstate.BindingRole, grant dbstate.MCPGrantKind, binding string) (AuthState, error) {
	row, final, err := principalOf(ctx, dbq.New(s.Pool), role, binding)
	if err != nil {
		return AuthState{}, err
	}
	if final && (!s.MergeTools || grant != dbstate.GrantWorkspace) {
		return AuthState{}, ErrInactive
	}
	return AuthState{Depth: int(row.AttemptDepth), MergeStatusOnly: final}, nil
}

// Resolve completes a principal built from an authenticated MCP actor. The main thread needs no
// lookup. A supervisor or child is checked again against the database, because its attempt may
// have been superseded since the request was authenticated.
func (s *Service) Resolve(ctx context.Context, p Principal) (Principal, error) {
	if !p.isTaskAgent() {
		return p, nil
	}
	row, final, err := principalOf(ctx, dbq.New(s.Pool), p.Role, p.BindingID)
	if errors.Is(err, ErrInactive) {
		return p, refusal(403, ReasonInactivePrincipal)
	}
	if err != nil {
		return p, err
	}
	if row.OwnerID != p.OwnerID {
		return p, refusal(403, ReasonInactivePrincipal)
	}
	p.TaskID, p.AttemptID, p.Final = row.TaskID, row.AttemptID, final
	return p, nil
}

// Identity is what whoami shows a supervisor or child about its task attempt.
type Identity struct {
	TaskID           string
	AttemptID        string
	RootTaskID       string
	ParentTaskID     *string
	WorkspaceID      *string
	WriterGeneration *int
	AttemptNumber    int
}

// Identity reads the task fields of a resolved supervisor or child principal.
func (s *Service) Identity(ctx context.Context, p Principal) (Identity, error) {
	q := dbq.New(s.Pool)
	t, err := q.GetTask(ctx, dbq.GetTaskParams{ID: p.TaskID, OwnerID: p.OwnerID})
	if err != nil {
		return Identity{}, notFoundOr(err, ReasonTaskNotFound)
	}
	a, err := q.GetTaskAttempt(ctx, p.AttemptID)
	if err != nil {
		return Identity{}, notFoundOr(err, ReasonTaskNotFound)
	}
	id := Identity{
		TaskID: t.ID, AttemptID: a.ID, RootTaskID: t.RootTaskID, ParentTaskID: t.ParentTaskID,
		WorkspaceID: a.WorkspaceID, AttemptNumber: int(a.Number),
	}
	if a.WriterGeneration != nil {
		g := int(*a.WriterGeneration)
		id.WriterGeneration = &g
	}
	return id, nil
}
