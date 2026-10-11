package merge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Loop carries merge candidates out: it evaluates the evidence until it holds, sends the one PUT
// from a committed merging row, and settles a merge whose outcome is unknown by reading the pull
// request.
func (s *Service) Loop() reconcile.Loop {
	return reconcile.Loop{
		Name: "merge-candidates", Table: Table(), Handle: s.handle, Lease: s.Lease, Wake: []string{Channel},
	}
}

func (s *Service) poll() time.Duration    { return orDefault(s.Poll, 15*time.Second) }
func (s *Service) ceiling() time.Duration { return orDefault(s.Ceiling, 30*time.Second) }
func (s *Service) window() time.Duration  { return orDefault(s.Window, 10*time.Minute) }

func (s *Service) recheckDelay(n int) time.Duration {
	d := orDefault(s.Recheck, 2*time.Second)
	for range min(n, 10) {
		d *= 2
		if d >= s.ceiling() {
			return s.ceiling()
		}
	}
	return min(d, s.ceiling())
}

func ptr[T any](v T) *T { return &v }

func (s *Service) handle(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	work, err := dbq.New(s.Pool).GetMergeWork(ctx, c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), nil
	}
	if err != nil {
		return reconcile.Retry(), fmt.Errorf("load candidate: %w", err)
	}
	repo, ok := repoOf(work.Repository)
	if !ok {
		return s.finish(ctx, c, work, dbstate.MergeBlocked, "invalid_repository", "")
	}
	var want Facts
	if err := json.Unmarshal(work.Facts, &want); err != nil {
		return s.finish(ctx, c, work, dbstate.MergeBlocked, "invalid_facts", "")
	}
	if dbstate.MergeState(c.State) == dbstate.MergeEvaluating {
		return s.evaluate(ctx, c, work, want, repo)
	}
	return s.settleUnknown(ctx, c, work, want, repo)
}

// evaluate reads the evidence and decides: finish, wait, or merge.
func (s *Service) evaluate(ctx context.Context, c reconcile.Claim, work dbq.GetMergeWorkRow, want Facts, repo github.Repo) (reconcile.Outcome, error) {
	if work.Route == dbstate.RouteApproval {
		if work.ReceiptActionID == nil {
			return s.finish(ctx, c, work, dbstate.MergeBlocked, "consent_missing", "")
		}
		state, err := dbq.New(s.Pool).GetResponseState(ctx, *work.ReceiptActionID)
		switch {
		case errors.Is(err, pgx.ErrNoRows) || (err == nil && state == "rejected_transport"):
			return s.finish(ctx, c, work, dbstate.MergeBlocked, "consent_withdrawn", "")
		case err != nil:
			return reconcile.Retry(), fmt.Errorf("read consent: %w", err)
		}
	}
	if work.Deadline != nil && !s.now().Before(*work.Deadline) {
		return s.finish(ctx, c, work, dbstate.MergeExpired, "deadline_passed", "")
	}
	ev, err := gather(ctx, s.GitHub, repo, int(work.PrNumber), s.now())
	switch st, hasStatus := github.StatusOf(err); {
	case err == nil:
	case hasStatus && st == 404:
		return s.finish(ctx, c, work, dbstate.MergeBlocked, "pull_request_not_found", "")
	default:
		// GitHub could not be read, so nothing is known: try again until the deadline.
		s.log().WarnContext(ctx, "merge evidence unavailable", "candidate", c.ID, "error", err)
		return reconcile.RetryAfter(s.poll()), nil
	}
	switch v, why := judge(ev, want, work.HeadSha); v {
	case verdictSuperseded:
		return s.finish(ctx, c, work, dbstate.MergeSuperseded, why, "")
	case verdictBlocked:
		return s.finish(ctx, c, work, dbstate.MergeBlocked, why, "")
	case verdictWait:
		return reconcile.RetryAfter(s.poll()), nil
	}
	return s.merge(ctx, c, work, want, repo)
}

// merge commits the merge intent and sends the one PUT. The row is merging before the PUT runs, so
// anything that stops this claim afterwards leaves a row that is only ever read, never resent.
// merge commits the merge intent and sends the one PUT. The row is merging before the PUT runs, so
// anything that stops this claim afterwards leaves a row that is only ever read, never resent.
//
// The evidence was read some time ago, so the decision is made again here. The remote reads (the
// trusted template mapping, the default branch's tip) come first, with no transaction open; then one
// short transaction takes the project, the consent response and the runtime binding in share mode
// and commits the intent only if every pinned fact still holds. The writers of those rows (policy
// changes, withdrawal, replacing or archiving a session) take them exclusively, so each either
// finishes before the decision reads it or waits for the decision to commit.
func (s *Service) merge(ctx context.Context, c reconcile.Claim, work dbq.GetMergeWorkRow, want Facts, repo github.Repo) (reconcile.Outcome, error) {
	if err := Transition(dbstate.MergeEvaluating, dbstate.MergeMerging); err != nil {
		return reconcile.Retry(), err
	}
	var pin Pin
	if work.Route == dbstate.RouteApproval {
		if err := json.Unmarshal(work.ConsentPin, &pin); err != nil || pin.ResponseID == "" {
			return s.finish(ctx, c, work, dbstate.MergeBlocked, "consent_missing", "")
		}
		_, mapped, err := s.mappingFor(ctx, pin.KagentSessionID, pin.agent(), pin.Provider)
		switch {
		case errors.Is(err, errUntrusted):
			return s.finish(ctx, c, work, dbstate.MergeBlocked, "consent_mapping_changed", "")
		case err != nil:
			s.log().WarnContext(ctx, "merge template mapping unavailable", "candidate", c.ID, "error", err)
			return reconcile.RetryAfter(s.poll()), nil
		case mapped.withResponse(pin.ResponseID) != pin:
			return s.finish(ctx, c, work, dbstate.MergeBlocked, "consent_mapping_changed", "")
		}
	}
	// A squash lands on the default branch's tip, so the tip the owner saw must still be the tip.
	tip, err := s.GitHub.BranchTip(ctx, repo, want.Base)
	if err != nil {
		s.log().WarnContext(ctx, "merge default branch unavailable", "candidate", c.ID, "error", readErr("default branch", err))
		return reconcile.RetryAfter(s.poll()), nil
	}
	if tip != want.BaseSHA {
		return s.finish(ctx, c, work, dbstate.MergeBlocked, "default_branch_moved", "")
	}

	q := dbq.New(s.Pool)
	owner := c.Owner
	var refusal string
	var final dbstate.MergeState
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		tq := dbq.New(tx)
		var err error
		if refusal, final, err = s.recheck(ctx, tq, work, want, pin); err != nil || refusal != "" {
			return err
		}
		n, err := tq.MarkMergeMerging(ctx, dbq.MarkMergeMergingParams{ID: c.ID, LeaseOwner: &owner, Attempts: attemptsOf(c)})
		if err != nil {
			return err
		}
		if n == 0 {
			return reconcile.ErrLeaseLost
		}
		return nil
	})
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		// Another candidate holds the merge intent for this pull request.
		return s.finish(ctx, c, work, dbstate.MergeBlocked, "merge_in_progress", "")
	case errors.Is(err, reconcile.ErrLeaseLost):
		return reconcile.Recorded(), err
	case err != nil:
		return reconcile.Retry(), err
	case refusal != "":
		return s.finish(ctx, c, work, final, refusal, "")
	}
	c.State = string(dbstate.MergeMerging)

	res, err := s.GitHub.MergePullRequest(ctx, repo, int(work.PrNumber), work.HeadSha)
	st, hasStatus := github.StatusOf(err)
	switch {
	case err == nil && res != nil && res.Merged && isSHA(res.SHA):
		return s.finish(ctx, c, work, dbstate.MergeMerged, "merged", res.SHA)
	case err == nil:
		return s.uncertain(ctx, c, "GitHub's answer did not say the pull request was merged")
	case hasStatus && st == 429:
		m, uerr := q.MarkMergeEvaluating(ctx, dbq.MarkMergeEvaluatingParams{ID: c.ID, LeaseOwner: &owner, Attempts: attemptsOf(c), Detail: ptr("rate limited")})
		if uerr != nil || m == 0 {
			return reconcile.Recorded(), orLost(uerr)
		}
		return reconcile.RetryAfter(s.poll()), nil
	case hasStatus && st >= 400 && st < 500:
		return s.finish(ctx, c, work, dbstate.MergeBlocked, fmt.Sprintf("merge_refused_%d", st), "")
	default:
		return s.uncertain(ctx, c, "the PUT's outcome is not known")
	}
}

func orLost(err error) error {
	if err != nil {
		return err
	}
	return reconcile.ErrLeaseLost
}

func isSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func (s *Service) uncertain(ctx context.Context, c reconcile.Claim, why string) (reconcile.Outcome, error) {
	owner := c.Owner
	m, err := dbq.New(s.Pool).MarkMergeUncertain(ctx, dbq.MarkMergeUncertainParams{ID: c.ID, LeaseOwner: &owner, Attempts: attemptsOf(c), Detail: &why})
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), reconcile.ErrLeaseLost
	}
	if err != nil {
		return reconcile.Retry(), err
	}
	return reconcile.RetryAfter(s.recheckDelay(int(m.Rechecks))), nil
}

// settleUnknown resolves a merge whose PUT may have run. It reads the pull request and never writes
// to GitHub.
func (s *Service) settleUnknown(ctx context.Context, c reconcile.Claim, work dbq.GetMergeWorkRow, want Facts, repo github.Repo) (reconcile.Outcome, error) {
	pr, err := s.GitHub.PullRequest(ctx, repo, int(work.PrNumber))
	if err != nil {
		// Unreadable: still unknown. Keep the row where it is and look again.
		s.log().WarnContext(ctx, "merge recheck unavailable", "candidate", c.ID, "error", err)
		return s.uncertain(ctx, c, "the pull request could not be read")
	}
	sameIdentity := pr.Number == int(work.PrNumber) && pr.Head.SHA == work.HeadSha && pr.Head.Ref == want.Head &&
		pr.Base.Ref == want.Base && pr.Head.RepoID == work.RepositoryID && pr.Base.RepoID == work.RepositoryID &&
		strings.EqualFold(pr.Base.RepoFullName, work.Repository)
	switch {
	case pr.Merged && sameIdentity && isSHA(pr.MergeCommitSHA):
		return s.finish(ctx, c, work, dbstate.MergeMerged, "merged", pr.MergeCommitSHA)
	case pr.Merged:
		return s.finish(ctx, c, work, dbstate.MergeBlocked, "merged_other_head", "")
	case pr.State != "open":
		return s.finish(ctx, c, work, dbstate.MergeBlocked, "closed_without_merge", "")
	case pr.Head.SHA != work.HeadSha:
		// The head moved, so the PUT (which carried the old head) can no longer succeed.
		return s.finish(ctx, c, work, dbstate.MergeBlocked, "head_moved", "")
	}
	// Open at the same head: the merge has not happened yet. It is only called not applied once the
	// window for a late PUT has passed, measured from when the outcome first became unknown.
	if s.now().Sub(work.UpdatedAt) >= s.window() {
		return s.finish(ctx, c, work, dbstate.MergeBlocked, "merge_not_applied", "")
	}
	return s.uncertain(ctx, c, "the pull request is still open at the merged head")
}

// finish writes a final state and the proposal's result in one transaction. It makes no network
// call.
func (s *Service) finish(ctx context.Context, c reconcile.Claim, work dbq.GetMergeWorkRow, state dbstate.MergeState, detail, mergeSHA string) (reconcile.Outcome, error) {
	if err := Transition(dbstate.MergeState(c.State), state); err != nil {
		return reconcile.Retry(), err
	}
	result, err := json.Marshal(map[string]any{
		"state": state, "detail": detail, "merge_sha": mergeSHA, "head_sha": work.HeadSha,
		"pr_number": work.PrNumber, "finished_at": s.now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return reconcile.Retry(), err
	}
	var sha *string
	if mergeSHA != "" {
		sha = &mergeSHA
	}
	owner := c.Owner
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		tq := dbq.New(tx)
		n, err := tq.FinishMergeCandidate(ctx, dbq.FinishMergeCandidateParams{
			ID: c.ID, LeaseOwner: &owner, Attempts: attemptsOf(c), State: state, Detail: &detail, MergeSha: sha,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return reconcile.ErrLeaseLost
		}
		return tq.InsertMergeResult(ctx, dbq.InsertMergeResultParams{ProposalID: work.ProposalID, State: state, Result: result})
	})
	if errors.Is(err, reconcile.ErrLeaseLost) {
		return reconcile.Recorded(), err
	}
	if err != nil {
		return reconcile.Retry(), err
	}
	return reconcile.Recorded(), nil
}

// CompletionSweep starts the evaluation of approval-route proposals whose owner approval is
// recorded but whose agent never made (or could not deliver) the approved call, for example because
// the session was restarted while the owner decided. The consent is the same one the tool call
// checks, so the sweep merges exactly what the call would have.
func (s *Service) CompletionSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "merge-approved-completion", Every: orDefault(s.SweepEvery, 30*time.Second), Run: s.Complete}
}

// Complete is one run of the completion sweep.
func (s *Service) Complete(ctx context.Context) error {
	q := dbq.New(s.Pool)
	rows, err := q.ListPreparedApprovalProposals(ctx, 100)
	if err != nil {
		return err
	}
	var firstErr error
	for _, r := range rows {
		if err := s.completeOne(ctx, r); err != nil {
			s.log().WarnContext(ctx, "merge completion failed", "proposal", r.ProposalID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (s *Service) completeOne(ctx context.Context, r dbq.ListPreparedApprovalProposalsRow) error {
	q := dbq.New(s.Pool)
	view, row, err := s.view(ctx, q, r.OwnerID, r.ProposalID)
	if err != nil {
		return err
	}
	// The sweep has the proposal's session as its actor and uses the approved tool's project gate.
	// This read is advisory; recheck locks and validates the authority again before dispatch.
	session, err := q.GetSessionRow(ctx, dbq.GetSessionRowParams{ID: r.SessionID, UserID: r.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if session.ProjectID == nil || ownsProject(*session.ProjectID, row.ProjectID) != nil {
		return nil
	}
	if len(view.Summary.UnavailableReasons) > 0 {
		return nil
	}
	consent, ok, err := s.consentFor(ctx, r.OwnerID, r.SessionID, r.ProposalID, "")
	if err != nil || !ok {
		return err
	}
	hash := digest(map[string]any{"proposal_id": r.ProposalID})
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		tq := dbq.New(tx)
		if err := tq.LockMergeScope(ctx, "approved:"+r.OwnerID+":"+consent.InvocationRequestID); err != nil {
			return err
		}
		if _, ok, err := replayIn(ctx, tq, r.OwnerID, dbstate.ToolApprovedMerge, consent.InvocationRequestID, hash); err != nil || ok {
			return err
		}
		return tq.InsertMergeRequestKey(ctx, dbq.InsertMergeRequestKeyParams{
			OwnerID: r.OwnerID, Tool: dbstate.ToolApprovedMerge, RequestID: consent.InvocationRequestID, PayloadHash: hash, ProposalID: r.ProposalID,
		})
	})
	if errors.Is(err, ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.begin(ctx, r.ProposalID, r.OwnerID, &consent)
}

// attemptsOf is the claim's attempt counter as the fenced queries take it.
func attemptsOf(c reconcile.Claim) int32 {
	return int32(c.Attempts) //nolint:gosec // a small counter
}

func (p Pin) withResponse(id string) Pin {
	p.ResponseID = id
	return p
}

// recheck is the DB half of the merge decision, run in the transaction that commits the intent. It
// takes the project, the consent response and the runtime binding in share mode and reports why the
// merge must not go ahead, with the final state to record, or "" when every pinned fact still holds.
func (s *Service) recheck(ctx context.Context, q *dbq.Queries, work dbq.GetMergeWorkRow, want Facts, pin Pin) (string, dbstate.MergeState, error) {
	project, err := q.LockProjectForMerge(ctx, dbq.LockProjectForMergeParams{ID: work.ProjectID, UserID: work.OwnerID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "project_unavailable", dbstate.MergeBlocked, nil
	case err != nil:
		return "", "", err
	case int(project.MergePolicyVersion) != want.PolicyVer:
		return "policy_changed", dbstate.MergeBlocked, nil
	}
	if work.Route == dbstate.RouteApproval {
		state, err := q.LockResponseForMerge(ctx, pin.ResponseID)
		switch {
		case errors.Is(err, pgx.ErrNoRows) || (err == nil && state == "rejected_transport"):
			return "consent_withdrawn", dbstate.MergeBlocked, nil
		case err != nil:
			return "", "", err
		}
		b, err := q.LockBindingForMerge(ctx, work.SessionID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return "consent_runtime_changed", dbstate.MergeBlocked, nil
		case err != nil:
			return "", "", err
		case b.UserID != work.OwnerID || b.ArchivedAt != nil || b.KagentDeletedAt != nil ||
			b.KagentSessionID == nil || *b.KagentSessionID != pin.KagentSessionID || b.Kind != pin.Provider:
			return "consent_runtime_changed", dbstate.MergeBlocked, nil
		case b.ProjectID == nil || ownsProject(*b.ProjectID, work.ProjectID) != nil:
			return "consent_project_changed", dbstate.MergeBlocked, nil
		}
	}
	if work.Deadline != nil && !s.now().Before(*work.Deadline) {
		return "deadline_passed", dbstate.MergeExpired, nil
	}
	return "", "", nil
}
