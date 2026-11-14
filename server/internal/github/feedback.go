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
