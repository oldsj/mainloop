package issues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
)

// MaxReviewComments bounds the inline comments of one review.
const MaxReviewComments = 30

// ReviewComment is one inline comment: a line of the diff on the RIGHT (new) or LEFT (old) side
// of a changed file.
type ReviewComment struct {
	Path string
	Line int
	Side string
	Body string
}

// CommentPullRequestInput is one comment_pull_request call. With no Comments it is a conversation
// comment; with Comments it is a COMMENT review of CommitSHA whose summary is Body.
type CommentPullRequestInput struct {
	OwnerID, ProjectID, SessionID, RequestID string
	Number                                   int
	Body                                     string
	CommitSHA                                string
	Comments                                 []ReviewComment
}

// CommentPullRequest records the intent to comment on, or review, a pull request and waits briefly
// for the loop to settle it. A review only ever comments: Mainloop never approves a pull request
// or requests changes on one, so an agent's review cannot satisfy a required review. Inline
// comments must name files the pull request changes.
func (s *Service) CommentPullRequest(ctx context.Context, in CommentPullRequestInput) (WriteResult, error) {
	kind := dbstate.IssueWritePRComment
	if len(in.Comments) > 0 {
		kind = dbstate.IssueWritePRReview
	}
	sent := make([]github.ReviewComment, 0, len(in.Comments))
	for _, c := range in.Comments {
		side := c.Side
		if side == "" {
			side = "RIGHT"
		}
		sent = append(sent, github.ReviewComment{Path: c.Path, Line: c.Line, Side: side, Body: c.Body})
	}
	key := "issue:" + in.OwnerID + ":" + in.RequestID
	hash := payloadHash(map[string]any{
		"kind": kind, "project_id": in.ProjectID, "body": in.Body, "issue_number": in.Number,
		"commit_sha": in.CommitSHA, "comments": sent,
	})
	if res, err := s.replay(ctx, key, hash); res != nil || err != nil {
		return deref(res), err
	}
	repo, detail, err := s.target(ctx, in.OwnerID, in.ProjectID)
	if err != nil {
		return WriteResult{}, err
	}
	if _, err := s.GitHub.PullRequest(ctx, repo, in.Number); errors.Is(err, github.ErrNotFound) {
		return WriteResult{}, &Refusal{"not_found", "no such pull request"}
	} else if err != nil {
		return WriteResult{}, unreadable(err)
	}
	p := dbq.InsertIssueWriteParams{
		IdempotencyKey: key, OwnerID: in.OwnerID, ProjectID: in.ProjectID, RequestID: in.RequestID, PayloadHash: hash,
		Kind: kind, RepoID: detail.ID, RepoFullName: detail.FullName,
		Labels: []string{}, Body: in.Body, IssueNumber: ptr(int64(in.Number)),
	}
	if kind == dbstate.IssueWritePRReview {
		if err := s.checkPaths(ctx, repo, in.Number, sent); err != nil {
			return WriteResult{}, err
		}
		raw, err := json.Marshal(sent)
		if err != nil {
			return WriteResult{}, err
		}
		p.CommitSha, p.ReviewComments = &in.CommitSHA, raw
	}
	return s.insert(ctx, p, in.SessionID)
}

// checkPaths refuses an inline comment on a file the pull request does not change, before
// anything is written: GitHub would refuse the whole review. A line outside the diff is still
// GitHub's to refuse (github_422).
func (s *Service) checkPaths(ctx context.Context, repo github.Repo, number int, comments []github.ReviewComment) error {
	files, complete, err := s.pullFiles(ctx, repo, number)
	if err != nil {
		return unreadable(err)
	}
	for _, c := range comments {
		if !slices.ContainsFunc(files, func(f github.PullFile) bool { return f.Filename == c.Path }) && complete {
			return &Refusal{"unknown_path", fmt.Sprintf("the pull request does not change %q", c.Path)}
		}
	}
	return nil
}
