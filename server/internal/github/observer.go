package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const observationMaxAge = 5 * time.Minute

var (
	// ErrObservationRetry means a read can be tried again on a later projection sweep.
	ErrObservationRetry = errors.New("github: observation retry later")
	// ErrObservationRefused means a read was refused; no observation can be trusted.
	ErrObservationRefused = errors.New("github: observation refused")
	// ErrObservationStale means collecting the observation exceeded its freshness window.
	ErrObservationStale = errors.New("github: observation stale")
)

// TaskObservation contains PR identity, untrusted body text and CI facts. The app wiring converts it to
// tasks.Observation: importing tasks here would create a cycle through workspaces and projects.
// Merge-policy evidence belongs to its own projection.
type TaskObservation struct {
	PRNumber  int
	PRURL     string
	PRHeadRef string
	PRRepo    string
	PRHeadSHA string
	PRBody    *string
	PRState   string
	CIState   string
	CIHeadSHA string
	// ObservedAt is captured when collection finishes, before persistence can wait on the DB.
	ObservedAt time.Time
}

// TaskObserver reads a task branch's most recent open PR, or its most recent closed/merged PR
// when none is open. It never caches observations or writes repository data. App tokens are
// scoped to each read's permissions. Nil App means observation is unavailable.
type TaskObserver struct {
	App *App
	// Now is the observation clock; nil uses time.Now. Set before first use.
	Now func() time.Time
}

// Observe reads every page before judging CI and re-reads the PR after collection. A moved
// head, identity or body is not observed. Freshness is the age of this read, not the age of the CI job:
// an old job freshly read for this exact head is valid. Persisted observations also need the
// consumer's five-minute age guard after this method returns.
func (o *TaskObserver) Observe(ctx context.Context, repository, branch string) (observation TaskObservation, err error) {
	if o == nil || o.App == nil {
		return TaskObservation{}, observationError(ErrUnavailable)
	}
	repo, err := ParseRepo(repository)
	if err != nil {
		return TaskObservation{}, observationError(err)
	}
	if _, err := branchPath(branch); err != nil {
		return TaskObservation{}, observationError(err)
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	started := now()
	ctx, cancel := context.WithTimeout(ctx, observationMaxAge)
	defer cancel()
	// Timestamp successful reads, including an absent PR, at collection finish. An absence
	// still needs a sweep time so it does not stay ahead of every other projection candidate.
	defer func() {
		if err != nil {
			return
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			observation, err = TaskObservation{}, observationError(ctxErr)
			return
		}
		observedAt := now()
		if age := observedAt.Sub(started); age < 0 || age > observationMaxAge {
			observation, err = TaskObservation{}, observationError(ErrObservationStale)
			return
		}
		observation.ObservedAt = observedAt
	}()
	// No base filter: a task's PR may target a branch other than the default branch.
	pulls, err := o.App.PullRequestsForHead(ctx, repo, branch, "")
	if err != nil {
		return absentOrError(err)
	}
	var chosen *PullRequestInfo
	for i := range pulls {
		p := &pulls[i]
		if !taskPR(p, repo, branch) {
			continue
		}
		if chosen == nil || (p.State == "open" && chosen.State != "open") ||
			((p.State == "open") == (chosen.State == "open") && p.Number > chosen.Number) {
			chosen = p
		}
	}
	if chosen == nil {
		return TaskObservation{}, nil
	}
	pr, err := o.App.PullRequest(ctx, repo, chosen.Number)
	if err != nil {
		return absentOrError(err)
	}
	if !taskPR(pr, repo, branch) {
		return TaskObservation{}, nil
	}
	ci, ciErr := o.readCI(ctx, repo, pr.Head.SHA)
	if ciErr != nil {
		// CI availability is independent of PR identity and body. Re-read the PR below before
		// keeping it, and discard every partial CI page. Cancellation still ends the observation.
		if ctx.Err() != nil {
			return TaskObservation{}, observationError(ctx.Err())
		}
		ci = "unknown"
	}
	fresh, err := o.App.PullRequest(ctx, repo, pr.Number)
	if err != nil {
		return absentOrError(err)
	}
	if !taskPR(fresh, repo, branch) || fresh.Head != pr.Head || fresh.Base != pr.Base ||
		fresh.State != pr.State || fresh.Merged != pr.Merged || fresh.Body != pr.Body ||
		fresh.BodyAvailable != pr.BodyAvailable {
		return TaskObservation{}, nil
	}
	state := pr.State
	if pr.Merged {
		state = "merged"
	}
	out := TaskObservation{
		PRNumber: pr.Number, PRURL: fmt.Sprintf("%s/pull/%d", repo.HTMLURL(), pr.Number),
		PRHeadRef: pr.Head.Ref, PRRepo: repo.FullName(), PRHeadSHA: pr.Head.SHA, PRState: state,
		CIState: ci,
	}
	if fresh.BodyAvailable {
		out.PRBody = &fresh.Body
	}
	if ci != "unknown" {
		out.CIHeadSHA = pr.Head.SHA
	}
	return out, nil
}

func taskPR(p *PullRequestInfo, repo Repo, branch string) bool {
	return p != nil && p.Number > 0 && p.Head.Ref == branch && !p.HeadRepoMissing &&
		strings.EqualFold(p.Head.RepoFullName, repo.FullName()) && strings.EqualFold(p.Base.RepoFullName, repo.FullName()) &&
		p.Head.RepoID > 0 && p.Head.RepoID == p.Base.RepoID && isSHA(p.Head.SHA) &&
		(p.State == "open" || p.State == "closed") && (!p.Merged || p.State == "closed")
}

func absentOrError(err error) (TaskObservation, error) {
	if observationAbsent(err) {
		return TaskObservation{}, nil
	}
	return TaskObservation{}, observationError(err)
}

func observationAbsent(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotInstalled)
}

func observationError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	status, hasStatus := StatusOf(err)
	var auth *AuthError
	if errors.As(err, &auth) && auth.Status != 0 {
		status, hasStatus = auth.Status, true
	}
	refused := hasStatus && status >= 400 && status < 500 && status != http.StatusTooManyRequests
	refused = refused || errors.Is(err, ErrInvalidRepo) || errors.Is(err, ErrBranchName) ||
		IsNotAllowed(err) || errors.Is(err, ErrInvalidToken)
	if refused {
		return fmt.Errorf("%w: %w", ErrObservationRefused, err)
	}
	return fmt.Errorf("%w: %w", ErrObservationRetry, err)
}

func (o *TaskObserver) readCI(ctx context.Context, repo Repo, sha string) (string, error) {
	runs, err := o.App.CheckRuns(ctx, repo, sha)
	if err != nil {
		if observationAbsent(err) {
			return "unknown", nil
		}
		return "unknown", err
	}
	statuses, err := o.App.CommitStatuses(ctx, repo, sha)
	if err != nil {
		if observationAbsent(err) {
			return "unknown", nil
		}
		return "unknown", err
	}
	return observedCI(runs, statuses), nil
}

func observedCI(runs []CheckRun, statuses []CommitStatus) string {
	type checkKey struct {
		app  int64
		name string
	}
	latestRuns := map[checkKey]CheckRun{}
	for _, r := range runs {
		k := checkKey{r.AppID, r.Name}
		if r.ID > latestRuns[k].ID {
			latestRuns[k] = r
		}
	}
	latestStatuses := map[string]CommitStatus{}
	for _, s := range statuses {
		old, ok := latestStatuses[s.Context]
		if !ok || s.CreatedAt.After(old.CreatedAt) || (s.CreatedAt.Equal(old.CreatedAt) && s.ID > old.ID) {
			latestStatuses[s.Context] = s
		}
	}
	if len(latestRuns)+len(latestStatuses) == 0 {
		return "unknown"
	}
	pending := false
	for _, r := range latestRuns {
		switch r.Status {
		case "completed":
			if r.Conclusion != "success" {
				return "failure"
			}
		case "queued", "in_progress", "pending", "waiting", "requested":
			pending = true
		default:
			return "failure"
		}
	}
	for _, s := range latestStatuses {
		switch s.State {
		case "success":
		case "pending":
			pending = true
		default:
			return "failure"
		}
	}
	if pending {
		return "pending"
	}
	return "success"
}
