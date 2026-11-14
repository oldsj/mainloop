package ci

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/oldsj/mainloop/server/internal/github"
)

// GitHubAPI is the part of the GitHub App the GitHub provider uses. It reads check runs and commit
// statuses, so it covers GitHub Actions and any other check provider that reports to GitHub.
type GitHubAPI interface {
	PullRequest(ctx context.Context, repo github.Repo, number int) (*github.PullRequestInfo, error)
	CheckRuns(ctx context.Context, repo github.Repo, sha string) ([]github.CheckRun, error)
	CommitStatuses(ctx context.Context, repo github.Repo, sha string) ([]github.CommitStatus, error)
	CheckRunAnnotations(ctx context.Context, repo github.Repo, runID int64) ([]github.Annotation, error)
}

// GitHub is the [Provider] over the GitHub App.
type GitHub struct{ API GitHubAPI }

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
// context on the commit.
func (g GitHub) Checks(ctx context.Context, repo Repo, sha string) ([]Check, error) {
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
