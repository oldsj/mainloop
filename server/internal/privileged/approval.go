package privileged

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/approval"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// ApprovalMaxAge bounds how long after the owner signed an approval the executor first acts on
// it. A retry of the same action finds its own use and is not bound by it.
const ApprovalMaxAge = 15 * time.Minute

// signedApproval, in serve-executor with approval keys pinned, checks the approval serve-api
// recorded against those keys, never against the database's: a pinned key signed it for this
// action's id and the digest just recomputed from the stored row, and its token has not approved
// another action. It returns why the action must not run, or "" to run it. An error is the
// database's; the lease lets the next run check again.
func (r *Runner) signedApproval(ctx context.Context, row dbq.PrivilegedAction, digest string) (string, error) {
	if !r.Executor || len(r.ApprovalKeys) == 0 {
		return "", nil
	}
	q := dbq.New(r.Pool)
	a, err := q.GetPrivilegedApproval(ctx, row.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "the approval was not signed with a Touch ID approval key", nil
	}
	if err != nil {
		return "", err
	}
	c, err := approval.Parse(a.Token, r.ApprovalKeys.Key, "")
	if err != nil {
		return "the approval's signature did not verify against a pinned key: " + err.Error(), nil
	}
	if c.ActionID != row.ID || c.Digest != digest {
		return "the signed approval names another action or digest", nil
	}
	use, err := q.UseExecutorApproval(ctx, dbq.UseExecutorApprovalParams{Jti: c.JTI, ActionID: row.ID})
	if err != nil {
		return "", err
	}
	if use.ActionID != row.ID {
		return fmt.Sprintf("the signed approval was already used for action %s", use.ActionID), nil
	}
	if use.Fresh {
		now := r.now()
		if c.IssuedAt.After(now.Add(approval.Skew)) {
			return "the signed approval is dated in the future", nil
		}
		if now.Sub(c.IssuedAt) > ApprovalMaxAge {
			return "the signed approval is older than 15 minutes", nil
		}
	}
	return "", nil
}
