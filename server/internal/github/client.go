package github

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v88/github"
)

// Metadata is what Mainloop caches about a repository.
type Metadata struct {
	Repo          Repo
	HTMLURL       string
	DefaultBranch string
	Description   *string
	AvatarURL     *string
	// OpenPRs and OpenIssues are separate counts: GitHub's own open_issues_count includes pull
	// requests, so OpenIssues is that count less OpenPRs.
	OpenPRs    int
	OpenIssues int
}

// PullRequest is one open pull request.
type PullRequest struct {
	Number     int
	Title      string
	State      string
	Author     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	URL        string
	IsMainloop bool
}

// Commit is one commit on a branch.
type Commit struct {
	SHA     string
	Message string
	Author  string
	Date    time.Time
	URL     string
}

const maxPage = 100

func clampLimit(n int) int {
	switch {
	case n < 1:
		return 1
	case n > maxPage:
		return maxPage
	}
	return n
}

// Metadata reads the repository's description, default branch, avatar and open counts. It uses
// a token limited to repository metadata and pull requests, read only.
func (a *App) Metadata(ctx context.Context, repo Repo) (*Metadata, error) {
	c, err := a.client(ctx, repo, Permissions{"metadata": Read, "pull_requests": Read})
	if err != nil {
		return nil, a.opaque(err)
	}
	r, _, err := c.Repositories.Get(ctx, repo.Owner, repo.Name)
	if err != nil {
		return nil, a.opaque(err)
	}
	// One open PR per page makes the last page number the PR count.
	prs, resp, err := c.PullRequests.List(ctx, repo.Owner, repo.Name, &gh.PullRequestListOptions{
		State: "open", ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil {
		return nil, a.opaque(err)
	}
	openPRs := len(prs)
	if resp.LastPage > 0 {
		openPRs = resp.LastPage
	}
	issues := max(r.GetOpenIssuesCount()-openPRs, 0)
	return &Metadata{
		Repo:          Repo{Owner: r.GetOwner().GetLogin(), Name: r.GetName()},
		HTMLURL:       r.GetHTMLURL(),
		DefaultBranch: r.GetDefaultBranch(),
		Description:   r.Description,
		AvatarURL:     r.GetOwner().AvatarURL,
		OpenPRs:       openPRs,
		OpenIssues:    issues,
	}, nil
}

// OpenPullRequests lists up to limit open pull requests, newest first. The token is limited to
// pull requests, read only.
func (a *App) OpenPullRequests(ctx context.Context, repo Repo, limit int) ([]PullRequest, error) {
	c, err := a.client(ctx, repo, Permissions{"pull_requests": Read})
	if err != nil {
		return nil, a.opaque(err)
	}
	prs, _, err := c.PullRequests.List(ctx, repo.Owner, repo.Name, &gh.PullRequestListOptions{
		State: "open", ListOptions: gh.ListOptions{PerPage: clampLimit(limit)},
	})
	if err != nil {
		return nil, a.opaque(err)
	}
	out := make([]PullRequest, 0, len(prs))
	for _, p := range prs {
		ref := p.GetHead().GetRef()
		out = append(out, PullRequest{
			Number:    p.GetNumber(),
			Title:     p.GetTitle(),
			State:     p.GetState(),
			Author:    p.GetUser().GetLogin(),
			CreatedAt: p.GetCreatedAt().Time,
			UpdatedAt: p.GetUpdatedAt().Time,
			URL:       p.GetHTMLURL(),
			// A branch made by a Mainloop session carries the product name or the feature prefix.
			IsMainloop: strings.Contains(strings.ToLower(ref), "mainloop") || strings.Contains(ref, "feature/"),
		})
	}
	return out, nil
}

// RecentCommits lists up to limit commits on branch, or on the default branch when branch is
// empty. The token is limited to contents, read only.
func (a *App) RecentCommits(ctx context.Context, repo Repo, branch string, limit int) ([]Commit, error) {
	c, err := a.client(ctx, repo, Permissions{"contents": Read})
	if err != nil {
		return nil, a.opaque(err)
	}
	commits, _, err := c.Repositories.ListCommits(ctx, repo.Owner, repo.Name, &gh.CommitsListOptions{
		SHA: branch, ListOptions: gh.ListOptions{PerPage: clampLimit(limit)},
	})
	if err != nil {
		return nil, a.opaque(err)
	}
	out := make([]Commit, 0, len(commits))
	for _, rc := range commits {
		sha := rc.GetSHA()
		if len(sha) > 7 {
			sha = sha[:7]
		}
		msg, _, _ := strings.Cut(rc.GetCommit().GetMessage(), "\n")
		out = append(out, Commit{
			SHA:     sha,
			Message: msg,
			Author:  rc.GetCommit().GetAuthor().GetName(),
			Date:    rc.GetCommit().GetAuthor().GetDate().Time,
			URL:     rc.GetHTMLURL(),
		})
	}
	return out, nil
}
