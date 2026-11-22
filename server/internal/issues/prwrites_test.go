package issues_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/issues"
)

const (
	prCommentsPath = "/repos/o/n/issues/42/comments"
	prReviewsPath  = "/repos/o/n/pulls/42/reviews"
)

// withPullFiles gives pull request 42 one changed file, x.go.
func (e *writeEnv) withPullFiles() {
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		for i := range r.PullRequests {
			if r.PullRequests[i].Number == 42 {
				r.PullRequests[i].Files = []githubtest.File{{Filename: "x.go", Status: "added", Additions: 2, Patch: "@@ -0,0 +1,2 @@\n+package x\n+var X = 1\n"}}
			}
		}
	})
}

func (e *writeEnv) pull(n int) githubtest.PullRequest {
	e.t.Helper()
	var out *githubtest.PullRequest
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		for _, p := range r.PullRequests {
			if p.Number == n {
				out = &p
			}
		}
	})
	if out == nil {
		e.t.Fatalf("no pull request #%d", n)
	}
	return *out
}

func (e *writeEnv) prComment(req string, number int) issues.CommentPullRequestInput {
	return issues.CommentPullRequestInput{OwnerID: owner, ProjectID: "p1", SessionID: "s1", RequestID: req, Number: number, Body: "Looks right"}
}

func (e *writeEnv) prReview(req string, path string) issues.CommentPullRequestInput {
	in := e.prComment(req, 42)
	in.Body, in.CommitSHA = "Two notes", tipSHA
	in.Comments = []issues.ReviewComment{{Path: path, Line: 2, Body: "Name it Y"}}
	return in
}

func TestCommentPullRequestPostsOnce(t *testing.T) {
	// Given a running write loop and an open pull request
	e := newWriteEnv(t)
	e.run()
	ctx := context.Background()

	// When a conversation comment is asked for, then asked for again with the same request id
	res, err := e.svc.CommentPullRequest(ctx, e.prComment("p1", 42))
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.CommentPullRequest(ctx, e.prComment("p1", 42))

	// Then it is posted once, through the issue comments endpoint, with its marker
	if res.State != dbstate.IssueWriteCreated || res.Kind != dbstate.IssueWritePRComment || res.Number != 42 || res.CommentID <= 0 ||
		!strings.Contains(res.URL, "/pull/42#issuecomment-") {
		t.Fatalf("res = %+v", res)
	}
	if err != nil || again != res || e.fake.Count("POST", prCommentsPath) != 1 {
		t.Fatalf("replay = %+v, %v, posts %d", again, err, e.fake.Count("POST", prCommentsPath))
	}
	fb := e.pull(42).Feedback
	if len(fb) != 1 || !strings.HasPrefix(fb[0].Body, "Looks right\n\n<!-- mainloop:issue-write ") {
		t.Fatalf("feedback = %+v", fb)
	}

	// And a number that is no pull request is refused before anything is written
	var r *issues.Refusal
	if _, err := e.svc.CommentPullRequest(ctx, e.prComment("p2", 999)); !errors.As(err, &r) || r.Code != "not_found" {
		t.Fatalf("missing pull request = %v", err)
	}
}

func TestReviewPullRequestOnlyComments(t *testing.T) {
	// Given a running write loop and a pull request that changes x.go
	e := newWriteEnv(t)
	e.withPullFiles()
	e.run()

	// When a review with an inline comment is asked for
	res, err := e.svc.CommentPullRequest(context.Background(), e.prReview("r1", "x.go"))
	if err != nil {
		t.Fatal(err)
	}

	// Then one COMMENT review pinned to the commit is submitted, never an approval
	if res.State != dbstate.IssueWriteCreated || res.Kind != dbstate.IssueWritePRReview || res.CommentID <= 0 ||
		!strings.Contains(res.URL, "#pullrequestreview-") {
		t.Fatalf("res = %+v", res)
	}
	sent := e.pull(42).Reviews
	if len(sent) != 1 || sent[0].Event != "COMMENT" || sent[0].CommitID != tipSHA || sent[0].Comments != 1 ||
		!strings.Contains(sent[0].Body, "<!-- mainloop:issue-write ") {
		t.Fatalf("reviews sent = %+v", sent)
	}
}

func TestReviewOnAFileThePullRequestDoesNotChangeIsRefused(t *testing.T) {
	// Given a pull request that changes only x.go
	e := newWriteEnv(t)
	e.withPullFiles()

	// When a review comments on y.go
	_, err := e.svc.CommentPullRequest(context.Background(), e.prReview("r1", "y.go"))

	// Then it is refused and nothing is written
	var r *issues.Refusal
	if !errors.As(err, &r) || r.Code != "unknown_path" {
		t.Fatalf("err = %v", err)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM issue_writes`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows = %d, %v", n, err)
	}
}

// The review took effect and its reply was lost: the loop finds it among the pull request's
// reviews by its marker and submits nothing twice.
func TestLostReviewReplyIsFoundByMarker(t *testing.T) {
	e := newWriteEnv(t)
	e.withPullFiles()
	e.fake.LoseReply("POST", prReviewsPath, 1)
	e.run()
	res, err := e.svc.CommentPullRequest(context.Background(), e.prReview("r1", "x.go"))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != dbstate.IssueWriteCreated || res.CommentID <= 0 || e.fake.Count("POST", prReviewsPath) != 1 || len(e.pull(42).Reviews) != 1 {
		t.Fatalf("res = %+v posts %d", res, e.fake.Count("POST", prReviewsPath))
	}
}

func TestLostPullCommentReplyIsFoundByMarker(t *testing.T) {
	e := newWriteEnv(t)
	e.fake.LoseReply("POST", prCommentsPath, 1)
	e.run()
	res, err := e.svc.CommentPullRequest(context.Background(), e.prComment("c1", 42))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != dbstate.IssueWriteCreated || res.CommentID <= 0 || e.fake.Count("POST", prCommentsPath) != 1 || len(e.pull(42).Feedback) != 1 {
		t.Fatalf("res = %+v posts %d", res, e.fake.Count("POST", prCommentsPath))
	}
}

func TestPullCommentTargetGoneBeforePostIsRefused(t *testing.T) {
	// Given a recorded comment whose pull request then disappears before the loop runs
	e := newWriteEnv(t)
	e.svc.Settle = 0
	res, err := e.svc.CommentPullRequest(context.Background(), e.prComment("c1", 42))
	if err != nil {
		t.Fatal(err)
	}
	e.fake.Update("o", "n", func(r *githubtest.Repository) { r.PullRequests = nil })

	// When the loop runs
	e.run()

	// Then the write is refused without posting
	e.waitState(res.ID, dbstate.IssueWriteRefused)
	if e.detail(res.ID) != "pull_request_missing" || e.fake.Count("POST", prCommentsPath) != 0 {
		t.Fatalf("detail %q posts %d", e.detail(res.ID), e.fake.Count("POST", prCommentsPath))
	}
}
