package tasks

import (
	"context"
	"errors"
	"fmt"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// BranchReader reads a repository's refs on origin and makes no changes. [github.App] is one.
type BranchReader interface {
	// BranchTip is the commit a branch points at; a missing branch is [github.ErrNotFound].
	BranchTip(ctx context.Context, repo github.Repo, branch string) (string, error)
	// CommitSHA resolves a branch, tag or commit to the full SHA of the commit it names.
	CommitSHA(ctx context.Context, repo github.Repo, ref string) (string, error)
}

// defaultBranchRef is the checkpoint ref of a successor that starts from the repository's default
// branch: a code task with no checkout ref, whose branch was never pushed.
const defaultBranchRef = "HEAD"

// checkpointError is a read of the branch head that can never succeed.
type checkpointError struct{ why string }

func (e *checkpointError) Error() string { return e.why }

// checkpoint records where a code successor's workspace starts, before any workspace call. It runs
// once the source attempt is fenced: the fence released the source's writer claim on the branch,
// which pushgate refuses while one of its pushes is unresolved, and the successor holds the claim
// now, so nothing moves the branch between this read and the workspace. The start is the task
// branch's head on origin; a branch that was never pushed starts from the task's base ref,
// resolved to its commit. The commit goes on the attempt (initial_ref) and the ref that was read
// on the operation (checkpoint_ref), in one transaction. A replay reads again and gets the same
// answer. A read that keeps failing ends the successor once the provisioning window is over, so a
// repository Mainloop lost access to is never taken for a branch that was never pushed.
func (s *Service) checkpoint(ctx context.Context, c reconcile.Claim, op dbq.TaskOperation) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
	if op.TargetAttemptID == nil {
		return reconcile.Done(string(dbstate.TaskOpBlocked)), nil
	}
	a, err := q.GetTaskAttempt(ctx, *op.TargetAttemptID)
	if err != nil {
		return reconcile.Outcome{}, err
	}
	t, err := q.GetTaskByID(ctx, a.TaskID)
	if err != nil {
		return reconcile.Outcome{}, err
	}
	if a.State != dbstate.AttemptCreating || TaskFinal(t.Status) {
		// A cancel got there first; its own operation settles the attempt.
		return s.stepAside(ctx, c, op)
	}
	ref, sha, err := s.readCheckpoint(ctx, q, t)
	var perm *checkpointError
	switch {
	case errors.As(err, &perm):
		return s.abort(ctx, c, op, a, "the branch head cannot be read: "+perm.why)
	case err != nil:
		if s.now().Sub(a.CreatedAt) > s.Limits.ProvisionWindow {
			return s.abort(ctx, c, op, a, "the branch head could not be read in time: "+err.Error())
		}
		return reconcile.Outcome{}, err
	}
	err = s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		t, err := q.GetTaskByIDForUpdate(ctx, a.TaskID)
		if err != nil {
			return nil, err
		}
		cur, err := q.GetTaskAttempt(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		// A cancel recorded during the read wins before any workspace is made; its own operation
		// settles the successor.
		cancelling, err := q.CancelPendingForTask(ctx, &t.ID)
		if err != nil {
			return nil, err
		}
		if cancelling || cur.State != dbstate.AttemptCreating || TaskFinal(t.Status) || t.CurrentAttemptID == nil || *t.CurrentAttemptID != a.ID {
			return nil, advance(ctx, q, c, op, dbstate.TaskOpBlocked, nil, nil)
		}
		if cur.InitialRef == nil {
			if _, err := q.SetAttemptInitialRef(ctx, dbq.SetAttemptInitialRefParams{ID: a.ID, InitialRef: &sha}); err != nil {
				return nil, err
			}
		}
		return nil, advanceWith(ctx, q, c, op, dbstate.TaskOpTargetCreating, nil, &a.ID, &ref)
	})
	if err != nil {
		return reconcile.Outcome{}, err
	}
	return reconcile.Recorded(), nil
}

// readCheckpoint reads where a code task's successor starts: refs/heads/<branch> and its commit
// when the task branch is on origin, otherwise the task's base ref and its commit. A base GitHub
// does not know either is an error to retry, not a reason to start anywhere else.
func (s *Service) readCheckpoint(ctx context.Context, q *dbq.Queries, t dbq.Task) (ref, sha string, err error) {
	if s.Branches == nil {
		return "", "", &checkpointError{"Mainloop has no GitHub App"}
	}
	if t.ProjectID == nil || t.CheckoutBranch == nil {
		return "", "", &checkpointError{"the task has no project or branch"}
	}
	full, err := q.GetProjectFullName(ctx, *t.ProjectID)
	if err != nil {
		return "", "", err
	}
	repo, err := github.ParseRepo(full)
	if err != nil {
		return "", "", &checkpointError{"the project is not a GitHub repository"}
	}
	branch := *t.CheckoutBranch
	tip, err := s.Branches.BranchTip(ctx, repo, branch)
	switch {
	case err == nil:
		return "refs/heads/" + branch, tip, nil
	case errors.Is(err, github.ErrBranchName):
		return "", "", &checkpointError{"the branch name cannot be read from GitHub"}
	case !errors.Is(err, github.ErrNotFound):
		return "", "", fmt.Errorf("read branch %s: %w", branch, err)
	}
	base := defaultBranchRef
	if t.CheckoutRef != nil && *t.CheckoutRef != "" {
		base = *t.CheckoutRef
	}
	sha, err = s.Branches.CommitSHA(ctx, repo, base)
	switch {
	case errors.Is(err, github.ErrBranchName):
		return "", "", &checkpointError{"the base ref cannot be read from GitHub"}
	case err != nil:
		return "", "", fmt.Errorf("branch %s is not on origin, and its base %s could not be read: %w", branch, base, err)
	}
	return base, sha, nil
}
