// Package privilegedtest is an in-memory GitHub for privileged-action tests.
package privilegedtest

import (
	"context"
	"fmt"
	"sync"

	"github.com/oldsj/mainloop/server/internal/github"
)

// GitHub holds pull requests by "owner/name#number" and records every write.
type GitHub struct {
	mu       sync.Mutex
	PRs      map[string]*github.PullRequestInfo
	Runs     []github.CheckRun
	Statuses []github.CommitStatus
	// MergeErr and CloseErr, when set, are returned once by the next write, after it applied when
	// Applied is set (a lost answer).
	MergeErr, CloseErr error
	Applied            bool
	Merges, Closes     []string
	Comments           []string
}

// New returns an empty GitHub.
func New() *GitHub { return &GitHub{PRs: map[string]*github.PullRequestInfo{}} }

func key(repo github.Repo, n int) string { return fmt.Sprintf("%s#%d", repo.FullName(), n) }

// Add stores an open pull request with the given head.
func (g *GitHub) Add(repo string, n int, head string) *github.PullRequestInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	yes := true
	p := &github.PullRequestInfo{
		Number: n, State: "open", Title: fmt.Sprintf("Change %d", n), Author: "octo-dev", Mergeable: &yes, MergeableState: "clean",
		HTMLURL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, n), Head: github.PRRef{Ref: fmt.Sprintf("feature-%d", n), SHA: head},
		Base: github.PRRef{Ref: "main"}, ChangedFiles: 2, Additions: 10, Deletions: 3,
	}
	g.PRs[fmt.Sprintf("%s#%d", repo, n)] = p
	return p
}

// Set changes a stored pull request under the lock.
func (g *GitHub) Set(repo string, n int, f func(*github.PullRequestInfo)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f(g.PRs[fmt.Sprintf("%s#%d", repo, n)])
}

func (g *GitHub) PullRequest(_ context.Context, repo github.Repo, n int) (*github.PullRequestInfo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.PRs[key(repo, n)]
	if !ok {
		return nil, github.ErrNotFound
	}
	c := *p
	return &c, nil
}

func (g *GitHub) CheckRuns(context.Context, github.Repo, string) ([]github.CheckRun, error) {
	return g.Runs, nil
}

func (g *GitHub) CommitStatuses(context.Context, github.Repo, string) ([]github.CommitStatus, error) {
	return g.Statuses, nil
}

func (g *GitHub) MergePullRequest(_ context.Context, repo github.Repo, n int, sha string) (*github.MergeResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.PRs[key(repo, n)]
	if !ok {
		return nil, github.ErrNotFound
	}
	if err := g.MergeErr; err != nil && !g.Applied {
		g.MergeErr = nil
		return nil, err
	}
	if p.Head.SHA != sha {
		return nil, &github.StatusError{Status: 409}
	}
	p.Merged, p.State, p.MergeCommitSHA = true, "closed", "f00dfeedf00dfeedf00dfeedf00dfeedf00dfeed"
	g.Merges = append(g.Merges, key(repo, n))
	if err := g.MergeErr; err != nil {
		g.MergeErr = nil
		return nil, err
	}
	return &github.MergeResult{Merged: true, SHA: p.MergeCommitSHA}, nil
}

func (g *GitHub) ClosePullRequest(_ context.Context, repo github.Repo, n int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.PRs[key(repo, n)]
	if !ok {
		return github.ErrNotFound
	}
	if err := g.CloseErr; err != nil && !g.Applied {
		g.CloseErr = nil
		return err
	}
	p.State = "closed"
	g.Closes = append(g.Closes, key(repo, n))
	if err := g.CloseErr; err != nil {
		g.CloseErr = nil
		return err
	}
	return nil
}

func (g *GitHub) CommentIssue(_ context.Context, repo github.Repo, n int, body string) (*github.IssueWrite, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Comments = append(g.Comments, key(repo, n)+": "+body)
	return &github.IssueWrite{Number: n, HTMLURL: fmt.Sprintf("https://github.com/%s/pull/%d#c", repo.FullName(), n)}, nil
}
