package github

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"
)

// Feedback kinds.
const (
	FeedbackReview        = "review"
	FeedbackReviewComment = "review_comment"
	FeedbackComment       = "comment"
)

// PRFeedback is one review, inline review comment or conversation comment on a pull request.
// Body is the author's text, untrusted.
type PRFeedback struct {
	ID   int64
	Kind string
	// Author is the login, AuthorType GitHub's user type ("User", "Bot"), and Association its
	// author_association (OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, NONE, ...).
	Author, AuthorType, Association string
	// State is a review's state; Path and Line place an inline comment.
	State string
	Path  string
	Line  int
	Body  string
	URL   string
	At    time.Time
}

// Trusted reports whether the item's author may steer an agent: the repository's owner, a member
// or a collaborator, or a bot (a GitHub App only comments where it is installed). Anyone else's
// text is counted, never read out.
func (f PRFeedback) Trusted() bool {
	return f.AuthorType == "Bot" || slices.Contains(trustedAssociations, f.Association)
}

// trustedAssociations are the author associations of people with write access to the repository.
var trustedAssociations = []string{"OWNER", "MEMBER", "COLLABORATOR"}

// feedbackPages bounds the reviews read (100 a page): the reviews listing has no since filter.
const feedbackPages = 3

type feedbackJSON struct {
	ID   int64 `json:"id"`
	User *struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	Association string     `json:"author_association"`
	State       string     `json:"state"`
	Path        string     `json:"path"`
	Line        *int       `json:"line"`
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	SubmittedAt *time.Time `json:"submitted_at"`
	CreatedAt   *time.Time `json:"created_at"`
}

func (f feedbackJSON) item(kind string) (PRFeedback, bool) {
	at := f.CreatedAt
	if kind == FeedbackReview {
		at = f.SubmittedAt // a pending review has none: it is not submitted yet
	}
	if at == nil || f.User == nil {
		return PRFeedback{}, false
	}
	out := PRFeedback{
		ID: f.ID, Kind: kind, Author: f.User.Login, AuthorType: f.User.Type, Association: f.Association,
		State: f.State, Path: f.Path, Body: f.Body, URL: f.HTMLURL, At: *at,
	}
	if f.Line != nil {
		out.Line = *f.Line
	}
	return out, true
}

// PullRequestFeedback lists what reviewers said on a pull request after since: its submitted
// reviews, inline review comments and conversation comments, oldest first. All three reads need
// only Pull requests read. Comments come one page of 100 after since; reviews, which GitHub cannot
// filter by time, at most three pages. Anything beyond those bounds comes on a later call with a
// later since.
func (a *App) PullRequestFeedback(ctx context.Context, repo Repo, number int, since time.Time) ([]PRFeedback, error) {
	perms := Permissions{"pull_requests": Read}
	n := strconv.Itoa(number)
	var out []PRFeedback
	keep := func(kind string, got []feedbackJSON) {
		for _, f := range got {
			if it, ok := f.item(kind); ok && it.At.After(since) {
				out = append(out, it)
			}
		}
	}
	for page := 1; page <= feedbackPages; page++ {
		var got []feedbackJSON
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := a.call(ctx, repo, perms, http.MethodGet, repoPath(repo, "pulls", n, "reviews")+"?"+q.Encode(), nil, &got); err != nil {
			return nil, err
		}
		keep(FeedbackReview, got)
		if len(got) < 100 {
			break
		}
	}
	q := url.Values{"per_page": {"100"}}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	for _, src := range []struct{ kind, path string }{
		{FeedbackReviewComment, repoPath(repo, "pulls", n, "comments")},
		{FeedbackComment, repoPath(repo, "issues", n, "comments")},
	} {
		var got []feedbackJSON
		if err := a.call(ctx, repo, perms, http.MethodGet, src.path+"?"+q.Encode(), nil, &got); err != nil {
			return nil, err
		}
		keep(src.kind, got)
	}
	slices.SortStableFunc(out, func(x, y PRFeedback) int { return x.At.Compare(y.At) })
	return out, nil
}

// ReviewComment is one inline comment of a review: a line of the diff on the new (RIGHT) or old
// (LEFT) side of a file, and the text.
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

type reviewJSON struct {
	ID          int64         `json:"id"`
	Body        *pullBody     `json:"body"`
	HTMLURL     string        `json:"html_url"`
	SubmittedAt *time.Time    `json:"submitted_at"`
	User        issueUserJSON `json:"user"`
}

func (r reviewJSON) write(number int) IssueWrite {
	w := IssueWrite{Number: number, CommentID: r.ID, HTMLURL: r.HTMLURL, Body: bodyText(r.Body), UserType: r.User.Type}
	if r.SubmittedAt != nil {
		w.CreatedAt = *r.SubmittedAt
	}
	return w
}

// CreateReview submits a review of the pull request at commitSHA with body and inline comments. The
// event is always COMMENT: Mainloop never approves or requests changes. The answer's CommentID is
// the review's id.
func (a *App) CreateReview(ctx context.Context, repo Repo, number int, commitSHA, body string, comments []ReviewComment) (*IssueWrite, error) {
	in := map[string]any{"commit_id": commitSHA, "body": body, "event": "COMMENT", "comments": comments}
	var out reviewJSON
	path := repoPath(repo, "pulls", strconv.Itoa(number), "reviews")
	if err := a.call(ctx, repo, Permissions{"pull_requests": Write}, http.MethodPost, path, in, &out); err != nil {
		return nil, err
	}
	if out.ID <= 0 || out.HTMLURL == "" {
		return nil, ErrUnavailable
	}
	w := out.write(number)
	return &w, nil
}

// ReviewsSince lists the pull request's submitted reviews at or after since, oldest first, reading
// at most [feedbackPages] pages of 100: GitHub cannot filter reviews by time. A listing that never
// ends within them is [ErrIncomplete].
func (a *App) ReviewsSince(ctx context.Context, repo Repo, number int, since time.Time) ([]IssueWrite, error) {
	var all []IssueWrite
	for page := 1; page <= feedbackPages; page++ {
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		var got []reviewJSON
		path := repoPath(repo, "pulls", strconv.Itoa(number), "reviews") + "?" + q.Encode()
		if err := a.call(ctx, repo, Permissions{"pull_requests": Read}, http.MethodGet, path, nil, &got); err != nil {
			return nil, err
		}
		for _, r := range got {
			if r.SubmittedAt != nil && !r.SubmittedAt.Before(since) {
				all = append(all, r.write(number))
			}
		}
		if len(got) < 100 {
			return all, nil
		}
	}
	return nil, ErrIncomplete
}
