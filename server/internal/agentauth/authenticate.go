package agentauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// ErrUnauthorized means the bearer does not identify a live agent: the token is unknown, its
// credential is not published, or the binding behind it is stale. The reason is deliberately not
// distinguished to the caller.
var ErrUnauthorized = errors.New("agentauth: unauthorized")

// DepthFunc resolves an actor's depth and whether it is limited to merge status. Task
// supervisors and children get theirs from the task principal. With a nil DepthFunc those two
// roles are refused, since nothing then checks their task principal.
type DepthFunc func(ctx context.Context, row dbq.GetMCPActorRow) (depth int, mergeStatusOnly bool, err error)

// Authenticator turns a bearer into an [Actor].
type Authenticator struct {
	Q     *dbq.Queries
	Depth DepthFunc
}

// roleDepth is the depth of a binding role before a task principal says otherwise.
func roleDepth(role dbstate.BindingRole) int {
	switch role {
	case dbstate.RoleSupervisor:
		return 1
	case dbstate.RoleChild:
		return 2
	}
	return 0
}

// Authenticate resolves the token in three steps, in order: the stored hash must name a
// published MCP credential, the binding and its session must still be live, and the binding's
// role and grant must be a valid pair. Any failure is [ErrUnauthorized].
func (a *Authenticator) Authenticate(ctx context.Context, token string) (Actor, error) {
	row, err := a.Q.GetMCPActor(ctx, HashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return Actor{}, ErrUnauthorized
	}
	if err != nil {
		return Actor{}, fmt.Errorf("agentauth: look up credential: %w", err)
	}
	if row.SessionArchivedAt != nil || stale(row.SessionStatus) {
		return Actor{}, ErrUnauthorized
	}
	if !ValidPair(row.Role, row.MCPGrantKind) {
		return Actor{}, ErrUnauthorized
	}
	if a.Depth == nil && (row.Role == dbstate.RoleSupervisor || row.Role == dbstate.RoleChild) {
		return Actor{}, ErrUnauthorized
	}
	actor := Actor{
		CredentialID: row.CredentialID, BindingID: row.BindingID, UserID: row.UserID,
		AgentKind: row.AgentKind, Role: row.Role, Grant: row.MCPGrantKind,
		Depth: roleDepth(row.Role),
	}
	if row.ProjectID != nil {
		actor.ProjectID = *row.ProjectID
	}
	if a.Depth != nil {
		depth, mergeOnly, err := a.Depth(ctx, row)
		if err != nil {
			return Actor{}, errors.Join(ErrUnauthorized, err)
		}
		actor.Depth, actor.MergeStatusOnly = depth, mergeOnly
	}
	return actor, nil
}

// stale reports whether a session in this status can no longer act. Idle, waiting and
// superseded sessions can still reach MCP; a finished session cannot.
func stale(s dbstate.SessionStatus) bool {
	switch s {
	case dbstate.SessionCompleted, dbstate.SessionFailed, dbstate.SessionCancelled:
		return true
	}
	return false
}

// LockPublished re-reads the credential behind actor FOR SHARE inside tx and requires it to be
// published, so a revoke (which updates the row) serializes against the write that follows. A
// credential that is no longer published is [ErrUnauthorized].
func LockPublished(ctx context.Context, tx pgx.Tx, actor Actor) error {
	if _, err := dbq.New(tx).LockPublishedCredential(ctx, actor.CredentialID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthorized
		}
		return fmt.Errorf("agentauth: lock credential: %w", err)
	}
	return nil
}

// Guarded returns ctx so that every [db.InTx] transaction started with it first takes
// [LockPublished] on actor's credential.
func Guarded(ctx context.Context, actor Actor) context.Context {
	return db.WithTxGuard(ctx, func(ctx context.Context, tx pgx.Tx) error { return LockPublished(ctx, tx, actor) })
}
