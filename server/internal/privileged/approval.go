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
// it.
const ApprovalMaxAge = 15 * time.Minute

// ApprovalRetryWindow is how long after its first use an approval can still be retried: every
// attempt under its longest deadline (a command's longest timeout on top of the remote calls'),
// the delays between them, and one lease's expiry after a crash.
const ApprovalRetryWindow = MaxAttempts*(RemoteTimeout+MaxCommandTimeout+retryDelay) + execLease

// ApprovalMaxUse bounds every use of an approval, first or retry, from the time the owner signed
// it. It is measured from the token's signed iat, which serve-api can't change, not from the
// executor's record of the first use, which it could.
const ApprovalMaxUse = ApprovalMaxAge + ApprovalRetryWindow

// executorRefusal is why the executor won't run an action. earlier is set when an earlier attempt passed
// the check, so it may have acted: the outcome is then uncertain, not failed.
type executorRefusal struct {
	msg     string
	earlier bool
}

// signedApproval, in serve-executor with approval keys pinned or required, checks the approval
// serve-api recorded against the pinned keys, never against the database's: a pinned key signed
// it for this action's id and the digest just recomputed from the stored row, its token has not
// approved another action, and it was signed recently enough for this use. It returns why the
// action must not run, or nil to run it. An error is the database's; the lease lets the next run
// check again.
func (r *Runner) signedApproval(ctx context.Context, row dbq.PrivilegedAction, digest string) (*executorRefusal, error) {
	if !r.Executor || (len(r.ApprovalKeys) == 0 && !r.ApprovalsRequired) {
		return nil, nil
	}
	if len(r.ApprovalKeys) == 0 {
		return &executorRefusal{msg: "signed approvals are required and no approval key is pinned"}, nil
	}
	q := dbq.New(r.Pool)
	a, err := q.GetPrivilegedApproval(ctx, row.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return &executorRefusal{msg: "the approval was not signed with a Touch ID approval key"}, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := approval.Parse(a.Token, r.ApprovalKeys.Key, "")
	if err != nil {
		return &executorRefusal{msg: "the approval's signature did not verify against a pinned key: " + err.Error()}, nil
	}
	if c.ActionID != row.ID || c.Digest != digest {
		return &executorRefusal{msg: "the signed approval names another action or digest"}, nil
	}
	now := r.now()
	if c.IssuedAt.After(now.Add(approval.Skew)) {
		return &executorRefusal{msg: "the signed approval is dated in the future"}, nil
	}
	use, err := q.UseExecutorApproval(ctx, dbq.UseExecutorApprovalParams{Jti: c.JTI, ActionID: row.ID})
	if err != nil {
		return nil, err
	}
	switch age := now.Sub(c.IssuedAt); {
	case use.ActionID != row.ID:
		return &executorRefusal{msg: fmt.Sprintf("the signed approval was already used for action %s", use.ActionID)}, nil
	case use.Fresh && age > ApprovalMaxAge:
		return &executorRefusal{msg: "the signed approval is older than 15 minutes"}, nil
	case !use.Fresh && age > ApprovalMaxUse:
		return &executorRefusal{
			msg:     fmt.Sprintf("the signed approval is older than %d minutes, too old for another attempt; approve a new request", int(ApprovalMaxUse.Minutes())),
			earlier: true,
		}, nil
	}
	return nil, nil
}
