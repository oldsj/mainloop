package githubtest

import (
	"net/http"
	"time"
)

// Feedback kinds, as GitHub serves them from three listings.
const (
	// FeedbackReview is a submitted review (GET /pulls/{n}/reviews).
	FeedbackReview = "review"
	// FeedbackReviewComment is an inline comment on the diff (GET /pulls/{n}/comments).
	FeedbackReviewComment = "review_comment"
	// FeedbackComment is a conversation comment (GET /issues/{n}/comments).
	FeedbackComment = "comment"
)

// Feedback is one review, inline review comment or conversation comment on a pull request.
type Feedback struct {
	ID   int64
	Kind string
	// Author is the login; AuthorType is "User" (the default) or "Bot"; Association is GitHub's
	// author_association (OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, NONE, ...).
	Author      string
	AuthorType  string
	Association string
	// State is a review's state (APPROVED, CHANGES_REQUESTED, COMMENTED, PENDING).
	State string
	// Path and Line place an inline comment.
	Path string
	Line int
	Body string
	At   time.Time
}

// feedbackCall serves one of the three listings for pull request num, oldest first, one page of
// up to per_page (default 30, at most 100). The issue comments listing honours since.
func (s *Server) feedbackCall(w http.ResponseWriter, r *http.Request, repo *Repository, num, kind string) {
	s.mu.Lock()
	i, ok := s.find(repo, num)
	if !ok {
		s.mu.Unlock()
		notFound(w)
		return
	}
	items := append([]Feedback(nil), repo.PullRequests[i].Feedback...)
	s.mu.Unlock()
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" && kind == FeedbackComment {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
			return
		}
		since = t
	}
	out := []map[string]any{}
	for _, f := range items {
		if f.Kind != kind || (!since.IsZero() && f.At.Before(since)) {
			continue
		}
		typ := f.AuthorType
		if typ == "" {
			typ = "User"
		}
		m := map[string]any{
			"id": f.ID, "body": f.Body, "author_association": f.Association,
			"user":     map[string]any{"login": f.Author, "type": typ},
			"html_url": "https://github.com/" + repo.Owner + "/" + repo.Name + "/pull/" + num,
		}
		at := f.At.UTC().Format(time.RFC3339)
		switch kind {
		case FeedbackReview:
			m["state"], m["submitted_at"] = f.State, at
		case FeedbackReviewComment:
			m["path"], m["line"], m["created_at"] = f.Path, f.Line, at
		default:
			m["created_at"] = at
		}
		out = append(out, m)
	}
	from, to, pageNo, last := page(r, len(out))
	s.link(w, r, pageNo, last)
	writeJSON(w, http.StatusOK, out[from:to])
}
