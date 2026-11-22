package github

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Issue is one GitHub issue as the read-only issue tool shows it.
type Issue struct {
	Number int
	Title  string
	Body   string
	// BodyAvailable distinguishes an observed empty body from null or missing text.
	BodyAvailable bool
	State         string
	Labels        []string
	HTMLURL       string
}

type issueJSON struct {
	Number int         `json:"number"`
	Title  string      `json:"title"`
	Body   *pullBody   `json:"body"`
	State  string      `json:"state"`
	Labels []labelJSON `json:"labels"`
	// PullRequest is set only when the issue number actually names a pull request: GitHub serves
	// both from the same endpoint.
	PullRequest *struct{} `json:"pull_request"`
	HTMLURL     string    `json:"html_url"`
}

// labelJSON accepts the one shape the REST API sends for a non-empty name: an object. A bare
// string label name was retired long ago and is not decoded.
type labelJSON struct {
	Name string `json:"name"`
}

// GetIssue reads one issue. A number that names a pull request, not an issue, is [ErrNotFound]:
// the two share a numbering space and an endpoint, but this reader serves issues only.
func (a *App) GetIssue(ctx context.Context, repo Repo, number int) (*Issue, error) {
	var out issueJSON
	if err := a.call(ctx, repo, Permissions{"issues": Read}, http.MethodGet, repoPath(repo, "issues", strconv.Itoa(number)), nil, &out); err != nil {
		return nil, err
	}
	if out.Number != number || out.PullRequest != nil {
		return nil, ErrNotFound
	}
	labels := make([]string, 0, len(out.Labels))
	for _, l := range out.Labels {
		labels = append(labels, l.Name)
	}
	issue := &Issue{Number: out.Number, Title: out.Title, State: out.State, Labels: labels, HTMLURL: out.HTMLURL}
	if out.Body != nil {
		issue.Body, issue.BodyAvailable = string(*out.Body), true
	}
	return issue, nil
}

// IssueWrite is an issue or issue comment Mainloop wrote, or found while looking for one: its
// number (the issue's), the comment's id (zero for an issue), the author's account type ("Bot" for
// the App) and the text, so a caller can recognise its own write by a marker in the body.
type IssueWrite struct {
	Number    int
	CommentID int64
	HTMLURL   string
	Body      string
	UserType  string
	CreatedAt time.Time
}

type issueUserJSON struct {
	Type string `json:"type"`
}

type issueListJSON struct {
	Number      int           `json:"number"`
	Body        *pullBody     `json:"body"`
	HTMLURL     string        `json:"html_url"`
	CreatedAt   time.Time     `json:"created_at"`
	User        issueUserJSON `json:"user"`
	PullRequest *struct{}     `json:"pull_request"`
}

type issueCommentJSON struct {
	ID        int64         `json:"id"`
	Body      *pullBody     `json:"body"`
	HTMLURL   string        `json:"html_url"`
	CreatedAt time.Time     `json:"created_at"`
	User      issueUserJSON `json:"user"`
}

func bodyText(b *pullBody) string {
	if b == nil {
		return ""
	}
	return string(*b)
}

// CreateIssue opens an issue. Labels must already exist in the repository: see [App.LabelNames].
func (a *App) CreateIssue(ctx context.Context, repo Repo, title, body string, labels []string) (*IssueWrite, error) {
	in := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		in["labels"] = labels
	}
	var out issueListJSON
	if err := a.call(ctx, repo, Permissions{"issues": Write}, http.MethodPost, repoPath(repo, "issues"), in, &out); err != nil {
		return nil, err
	}
	if out.Number <= 0 || out.HTMLURL == "" || out.PullRequest != nil {
		return nil, ErrUnavailable
	}
	return &IssueWrite{Number: out.Number, HTMLURL: out.HTMLURL, Body: bodyText(out.Body), UserType: out.User.Type, CreatedAt: out.CreatedAt}, nil
}

// CommentIssue adds a comment to an issue.
func (a *App) CommentIssue(ctx context.Context, repo Repo, number int, body string) (*IssueWrite, error) {
	var out issueCommentJSON
	path := repoPath(repo, "issues", strconv.Itoa(number), "comments")
	if err := a.call(ctx, repo, Permissions{"issues": Write}, http.MethodPost, path, map[string]any{"body": body}, &out); err != nil {
		return nil, err
	}
	if out.ID <= 0 || out.HTMLURL == "" {
		return nil, ErrUnavailable
	}
	return &IssueWrite{
		Number: number, CommentID: out.ID, HTMLURL: out.HTMLURL, Body: bodyText(out.Body), UserType: out.User.Type, CreatedAt: out.CreatedAt,
	}, nil
}

// IssuesCreatedSince lists the issues (not pull requests) created at or after since, newest
// first. It reads newest-created pages until one reaches past since; a listing that never does
// within the pages it reads is [ErrIncomplete].
func (a *App) IssuesCreatedSince(ctx context.Context, repo Repo, since time.Time) ([]IssueWrite, error) {
	var all []IssueWrite
	for page := 1; page <= listMaxPages; page++ {
		q := url.Values{
			"state": {"all"}, "sort": {"created"}, "direction": {"desc"},
			"per_page": {strconv.Itoa(listPerPage)}, "page": {strconv.Itoa(page)},
		}
		var got []issueListJSON
		if err := a.call(ctx, repo, Permissions{"issues": Read}, http.MethodGet, repoPath(repo, "issues")+"?"+q.Encode(), nil, &got); err != nil {
			return nil, err
		}
		if len(got) > listPerPage {
			return nil, ErrUnavailable
		}
		for _, i := range got {
			if i.CreatedAt.Before(since) {
				return all, nil
			}
			if i.PullRequest == nil {
				all = append(all, IssueWrite{Number: i.Number, HTMLURL: i.HTMLURL, Body: bodyText(i.Body), UserType: i.User.Type, CreatedAt: i.CreatedAt})
			}
		}
		if len(got) < listPerPage {
			return all, nil
		}
	}
	return nil, ErrIncomplete
}

// IssueCommentsSince lists an issue's comments updated at or after since, oldest first, with an
// Issues read token.
func (a *App) IssueCommentsSince(ctx context.Context, repo Repo, number int, since time.Time) ([]IssueWrite, error) {
	return a.commentsSince(ctx, repo, number, since, Permissions{"issues": Read})
}

// PullCommentsSince lists a pull request's conversation comments updated at or after since, oldest
// first, with a Pull requests read token.
func (a *App) PullCommentsSince(ctx context.Context, repo Repo, number int, since time.Time) ([]IssueWrite, error) {
	return a.commentsSince(ctx, repo, number, since, Permissions{"pull_requests": Read})
}

func (a *App) commentsSince(ctx context.Context, repo Repo, number int, since time.Time, perms Permissions) ([]IssueWrite, error) {
	var all []IssueWrite
	for page := 1; page <= listMaxPages; page++ {
		q := url.Values{
			"since": {since.UTC().Format(time.RFC3339)}, "per_page": {strconv.Itoa(listPerPage)}, "page": {strconv.Itoa(page)},
		}
		var got []issueCommentJSON
		path := repoPath(repo, "issues", strconv.Itoa(number), "comments") + "?" + q.Encode()
		if err := a.call(ctx, repo, perms, http.MethodGet, path, nil, &got); err != nil {
			return nil, err
		}
		if len(got) > listPerPage {
			return nil, ErrUnavailable
		}
		for _, c := range got {
			all = append(all, IssueWrite{
				Number: number, CommentID: c.ID, HTMLURL: c.HTMLURL, Body: bodyText(c.Body), UserType: c.User.Type, CreatedAt: c.CreatedAt,
			})
		}
		if len(got) < listPerPage {
			return all, nil
		}
	}
	return nil, ErrIncomplete
}

// LabelNames lists the names of the repository's labels.
func (a *App) LabelNames(ctx context.Context, repo Repo) ([]string, error) {
	var all []string
	for page := 1; page <= listMaxPages; page++ {
		q := url.Values{"per_page": {strconv.Itoa(listPerPage)}, "page": {strconv.Itoa(page)}}
		var got []labelJSON
		if err := a.call(ctx, repo, Permissions{"issues": Read}, http.MethodGet, repoPath(repo, "labels")+"?"+q.Encode(), nil, &got); err != nil {
			return nil, err
		}
		if len(got) > listPerPage {
			return nil, ErrUnavailable
		}
		for _, l := range got {
			all = append(all, l.Name)
		}
		if len(got) < listPerPage {
			return all, nil
		}
	}
	return nil, ErrIncomplete
}
