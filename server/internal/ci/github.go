package ci

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/oldsj/mainloop/server/internal/github"
)

// GitHubAPI is the part of the GitHub App the GitHub provider uses. It reads check runs and commit
// statuses, so it covers GitHub Actions and any other check provider that reports to GitHub.
type GitHubAPI interface {
	PullRequest(ctx context.Context, repo github.Repo, number int) (*github.PullRequestInfo, error)
	BranchTip(ctx context.Context, repo github.Repo, branch string) (string, error)
	RecentlyMergedPullRequests(ctx context.Context, repo github.Repo, base string, limit int) ([]github.MergedPullRequest, error)
	CheckSuites(ctx context.Context, repo github.Repo, sha string) ([]github.CheckSuite, error)
	CheckRuns(ctx context.Context, repo github.Repo, sha string) ([]github.CheckRun, error)
	CommitStatuses(ctx context.Context, repo github.Repo, sha string) ([]github.CommitStatus, error)
	CheckRunAnnotations(ctx context.Context, repo github.Repo, runID int64) ([]github.Annotation, error)
}

// GitHub is the [Provider] over the GitHub App. Now is the clock for the abandoned-suite rule;
// nil means time.Now.
type GitHub struct {
	API GitHubAPI
	Now func() time.Time
}

// abandonedSuiteGrace is how long a queued suite with no runs may sit before it stops counting as
// pending: some apps create a suite on every push and never fill it. The merge reader uses the same.
const abandonedSuiteGrace = 10 * time.Minute

func ghRepo(r Repo) github.Repo { return github.Repo{Owner: r.Owner, Name: r.Name} }

// HeadSHA resolves the pull request to its current head commit.
func (g GitHub) HeadSHA(ctx context.Context, repo Repo, pr int) (string, error) {
	info, err := g.API.PullRequest(ctx, ghRepo(repo), pr)
	if errors.Is(err, github.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return info.Head.SHA, nil
}

// BranchHead resolves a branch to its tip commit.
func (g GitHub) BranchHead(ctx context.Context, repo Repo, branch string) (string, error) {
	sha, err := g.API.BranchTip(ctx, ghRepo(repo), branch)
	if errors.Is(err, github.ErrNotFound) {
		return "", ErrNotFound
	}
	return sha, err
}

// MergedHeads lists the heads of the pull requests most recently merged into branch. It reads
// the limit most recently updated closed pull requests, so fewer may come back.
func (g GitHub) MergedHeads(ctx context.Context, repo Repo, branch string, limit int) ([]MergedHead, error) {
	prs, err := g.API.RecentlyMergedPullRequests(ctx, ghRepo(repo), branch, limit)
	if err != nil {
		return nil, err
	}
	out := make([]MergedHead, 0, len(prs))
	for _, p := range prs {
		out = append(out, MergedHead{PR: p.Number, SHA: p.HeadSHA})
	}
	return out, nil
}

// unknownCommit reports an answer that means GitHub has no such commit: 404, or the 422 ("No
// commit found for SHA") the check-runs listing gives instead.
func unknownCommit(err error) bool {
	if errors.Is(err, github.ErrNotFound) {
		return true
	}
	st, ok := github.StatusOf(err)
	return ok && st == http.StatusUnprocessableEntity
}

// Checks lists the latest check run of each (app, name) and the latest commit status of each
// context on the commit. A check suite that has not completed and has no pending run of its own is
// listed as a pending check too: a workflow's dependent jobs get their check runs only when they
// are queued, so between jobs the suite is the only sign that more is coming.
func (g GitHub) Checks(ctx context.Context, repo Repo, sha string) ([]Check, error) {
	suites, err := g.API.CheckSuites(ctx, ghRepo(repo), sha)
	if unknownCommit(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	runs, err := g.API.CheckRuns(ctx, ghRepo(repo), sha)
	if unknownCommit(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	statuses, err := g.API.CommitStatuses(ctx, ghRepo(repo), sha)
	if unknownCommit(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	type key struct {
		app  int64
		name string
	}
	latestRun := map[key]github.CheckRun{}
	for _, r := range runs {
		if k := (key{r.AppID, r.Name}); r.ID >= latestRun[k].ID {
			latestRun[k] = r
		}
	}
	latestStatus := map[string]github.CommitStatus{}
	for _, s := range statuses {
		if old, ok := latestStatus[s.Context]; !ok || s.CreatedAt.After(old.CreatedAt) || (s.CreatedAt.Equal(old.CreatedAt) && s.ID > old.ID) {
			latestStatus[s.Context] = s
		}
	}
	out := make([]Check, 0, len(latestRun)+len(latestStatus))
	withRuns, runPending := map[int64]bool{}, map[int64]bool{}
	for _, r := range latestRun {
		withRuns[r.SuiteID] = true
		runPending[r.SuiteID] = runPending[r.SuiteID] || RunResult(r.Status, r.Conclusion) == Pending
	}
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	for _, s := range suites {
		if s.Status == "completed" || runPending[s.ID] || abandoned(s, withRuns[s.ID], now()) {
			continue
		}
		name := s.AppSlug
		if name == "" {
			name = "check suite"
		}
		out = append(out, Check{ID: s.ID, Kind: KindCheckSuite, Name: name, App: s.AppSlug, Status: s.Status, Result: Pending})
	}
	for _, r := range latestRun {
		out = append(out, Check{
			ID: r.ID, Kind: KindCheckRun, Name: r.Name, App: r.AppSlug, Status: statusText(r.Status, r.Conclusion),
			Result: RunResult(r.Status, r.Conclusion), URL: r.HTMLURL, StartedAt: r.StartedAt, CompletedAt: r.CompletedAt,
			Summary: runSummary(r), Text: r.OutputText,
		})
	}
	for _, s := range latestStatus {
		out = append(out, Check{
			ID: s.ID, Kind: KindCommitStatus, Name: s.Context, Status: s.State, Result: StatusResult(s.State),
			URL: s.TargetURL, Summary: s.Description,
		})
	}
	slices.SortFunc(out, func(a, b Check) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Kind+"\x00"+a.App, b.Kind+"\x00"+b.App)
	})
	return out, nil
}

// Failure reads a failed check's output and, for a check run, its annotations. It reads no log:
// GitHub serves Actions job logs only to a token with actions:read, which the App does not ask for
// yet.
func (g GitHub) Failure(ctx context.Context, repo Repo, c Check) (Failure, error) {
	f := Failure{Name: c.Name, URL: c.URL, Summary: c.Summary, Text: c.Text}
	if c.Kind != KindCheckRun {
		return f, nil
	}
	list, err := g.API.CheckRunAnnotations(ctx, ghRepo(repo), c.ID)
	if errors.Is(err, github.ErrNotFound) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	for _, a := range list {
		f.Annotations = append(f.Annotations, Annotation{
			Path: a.Path, StartLine: a.StartLine, EndLine: a.EndLine, Level: a.Level, Title: a.Title, Message: a.Message,
		})
	}
	return f, nil
}

// abandoned reports a queued suite its app never filled: no runs, none reported, and older than
// the grace period.
func abandoned(s github.CheckSuite, hasRuns bool, now time.Time) bool {
	if s.Status != "queued" || hasRuns || (s.LatestCheckRunsCount != nil && *s.LatestCheckRunsCount != 0) {
		return false
	}
	created, err := time.Parse(time.RFC3339, s.CreatedAt)
	return err == nil && now.Sub(created) > abandonedSuiteGrace
}

func statusText(status, conclusion string) string {
	if status == "completed" && conclusion != "" {
		return conclusion
	}
	return status
}

// runSummary joins a check run's own output title and summary.
func runSummary(r github.CheckRun) string {
	switch {
	case r.OutputTitle == "":
		return r.OutputSummary
	case r.OutputSummary == "":
		return r.OutputTitle
	}
	return r.OutputTitle + "\n\n" + r.OutputSummary
}
