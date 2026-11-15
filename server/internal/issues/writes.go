package issues

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Channel wakes the write loop when a row is written.
const Channel = "mainloop_issue_writes"

// MaxLabels bounds the labels of a new issue.
const MaxLabels = 10

// ErrConflict means the request id was used with other arguments.
var ErrConflict = errors.New("issues: request id reused with different arguments")

// lookback is how far before the intent was written a lookup reaches, for clock skew between
// PostgreSQL and GitHub.
const lookback = 2 * time.Minute

// Table is the reconcile table of issue writes.
func Table() reconcile.Table {
	return reconcile.NewTable("issue_writes",
		string(dbstate.IssueWritePending), string(dbstate.IssueWriteDispatching), string(dbstate.IssueWriteUncertain))
}

// Loop carries issue writes out. It runs in the API process; the MCP process only writes rows.
func (s *Service) Loop() reconcile.Loop {
	return reconcile.Loop{
		Name: "issue-writes", Table: Table(), Handle: s.handle, Lease: s.Lease, Wake: []string{Channel},
	}
}

// CreateInput is one create_issue call.
type CreateInput struct {
	OwnerID, ProjectID, SessionID, RequestID string
	Title, Body                              string
	Labels                                   []string
}

// CommentInput is one comment_issue call.
type CommentInput struct {
	OwnerID, ProjectID, SessionID, RequestID string
	Number                                   int
	Body                                     string
}

// WriteResult is the state of an issue write as the agent sees it.
type WriteResult struct {
	ID         string
	Kind       dbstate.IssueWriteKind
	State      dbstate.IssueWriteState
	Repository string
	// Number is the issue: a comment's target, or a new issue's number once created.
	Number    int64
	CommentID int64
	URL       string
	Detail    string
}

func writeResultOf(r dbq.IssueWrite) WriteResult {
	out := WriteResult{ID: r.ID, Kind: r.Kind, State: r.State, Repository: r.RepoFullName}
	if r.IssueNumber != nil {
		out.Number = *r.IssueNumber
	}
	if r.CommentID != nil {
		out.CommentID = *r.CommentID
	}
	if r.Url != nil {
		out.URL = *r.Url
	}
	if r.Detail != nil {
		out.Detail = *r.Detail
	}
	return out
}

// Final reports whether a write can no longer change.
func Final(s dbstate.IssueWriteState) bool {
	return s == dbstate.IssueWriteCreated || s == dbstate.IssueWriteRefused
}

// marker is the line a write's body ends with. It is how a lookup recognises the write after a
// lost answer: the row id is unknown to anyone before the row exists. GitHub does not render an
// HTML comment.
func marker(id string) string { return "<!-- mainloop:issue-write " + id + " -->" }

func withMarker(body, id string) string {
	if body == "" {
		return marker(id)
	}
	return body + "\n\n" + marker(id)
}

func writeHash(kind dbstate.IssueWriteKind, projectID, title, body string, labels []string, number int) string {
	if labels == nil {
		labels = []string{}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{
		"kind": kind, "project_id": projectID, "title": title, "body": body, "labels": labels, "issue_number": number,
	})
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:])
}

// replay returns the row already written under key, or nil. The same key with other arguments is
// [ErrConflict].
func (s *Service) replay(ctx context.Context, key, hash string) (*WriteResult, error) {
	row, err := dbq.New(s.Pool).GetIssueWriteByKey(ctx, key)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	case row.PayloadHash != hash:
		return nil, ErrConflict
	}
	res, err := s.settle(ctx, row.ID)
	return &res, err
}

// target resolves the project to its repository and reads the repository's identity, refusing a
// repository GitHub now names differently.
func (s *Service) target(ctx context.Context, ownerID, projectID string) (github.Repo, *github.RepoDetail, error) {
	repo, err := s.project(ctx, ownerID, projectID)
	if err != nil {
		return github.Repo{}, nil, err
	}
	detail, err := s.GitHub.RepoDetail(ctx, repo)
	if err != nil {
		return github.Repo{}, nil, &Refusal{"github_unavailable", "GitHub could not be read: " + statusText(err)}
	}
	if !strings.EqualFold(detail.FullName, repo.Owner+"/"+repo.Name) {
		return github.Repo{}, nil, &Refusal{"repository_mismatch", "the repository is not the project's"}
	}
	return repo, detail, nil
}

// Create records the intent to open an issue and waits briefly for the loop to settle it. Labels
// must exist in the repository; they are matched without regard to case and sent as the
// repository spells them.
func (s *Service) Create(ctx context.Context, in CreateInput) (WriteResult, error) {
	key := "issue:" + in.OwnerID + ":" + in.RequestID
	hash := writeHash(dbstate.IssueWriteIssue, in.ProjectID, in.Title, in.Body, in.Labels, 0)
	if res, err := s.replay(ctx, key, hash); res != nil || err != nil {
		return deref(res), err
	}
	repo, detail, err := s.target(ctx, in.OwnerID, in.ProjectID)
	if err != nil {
		return WriteResult{}, err
	}
	labels, err := s.labels(ctx, repo, in.Labels)
	if err != nil {
		return WriteResult{}, err
	}
	return s.insert(ctx, dbq.InsertIssueWriteParams{
		IdempotencyKey: key, OwnerID: in.OwnerID, ProjectID: in.ProjectID, RequestID: in.RequestID, PayloadHash: hash,
		Kind: dbstate.IssueWriteIssue, RepoID: detail.ID, RepoFullName: detail.FullName,
		Title: in.Title, Labels: labels, Body: in.Body,
	}, in.SessionID)
}

// Comment records the intent to comment on an issue and waits briefly for the loop to settle it.
// The number must name an issue, not a pull request.
func (s *Service) Comment(ctx context.Context, in CommentInput) (WriteResult, error) {
	key := "issue:" + in.OwnerID + ":" + in.RequestID
	hash := writeHash(dbstate.IssueWriteComment, in.ProjectID, "", in.Body, nil, in.Number)
	if res, err := s.replay(ctx, key, hash); res != nil || err != nil {
		return deref(res), err
	}
	repo, detail, err := s.target(ctx, in.OwnerID, in.ProjectID)
	if err != nil {
		return WriteResult{}, err
	}
	if _, err := s.GitHub.GetIssue(ctx, repo, in.Number); errors.Is(err, github.ErrNotFound) {
		return WriteResult{}, &Refusal{"not_found", "no such issue"}
	} else if err != nil {
		return WriteResult{}, &Refusal{"github_unavailable", "GitHub could not be read: " + statusText(err)}
	}
	number := int64(in.Number)
	return s.insert(ctx, dbq.InsertIssueWriteParams{
		IdempotencyKey: key, OwnerID: in.OwnerID, ProjectID: in.ProjectID, RequestID: in.RequestID, PayloadHash: hash,
		Kind: dbstate.IssueWriteComment, RepoID: detail.ID, RepoFullName: detail.FullName,
		Labels: []string{}, Body: in.Body, IssueNumber: &number,
	}, in.SessionID)
}

func deref(r *WriteResult) WriteResult {
	if r == nil {
		return WriteResult{}
	}
	return *r
}

// labels maps the asked-for labels onto the repository's own, refusing one it does not have.
func (s *Service) labels(ctx context.Context, repo github.Repo, asked []string) ([]string, error) {
	out := []string{}
	if len(asked) == 0 {
		return out, nil
	}
	have, err := s.GitHub.LabelNames(ctx, repo)
	if err != nil {
		return nil, &Refusal{"github_unavailable", "GitHub could not be read: " + statusText(err)}
	}
	byFold := map[string]string{}
	for _, name := range have {
		byFold[strings.ToLower(name)] = name
	}
	seen := map[string]bool{}
	for _, l := range asked {
		name, ok := byFold[strings.ToLower(l)]
		if !ok {
			return nil, &Refusal{"unknown_label", fmt.Sprintf("the repository has no label %q", l)}
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

// insert writes the intent with its marker, wakes the loop and waits for it.
func (s *Service) insert(ctx context.Context, p dbq.InsertIssueWriteParams, sessionID string) (WriteResult, error) {
	p.ID = uuid.NewString()
	p.Body = withMarker(p.Body, p.ID)
	if sessionID != "" {
		p.SessionID = &sessionID
	}
	// The intent is the tool call's write, so it runs in a guarded transaction (the MCP credential
	// is re-checked in it); the GitHub reads before it stay outside any transaction.
	var n int64
	if err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		n, err = dbq.New(tx).InsertIssueWrite(ctx, p)
		return err
	}); err != nil {
		return WriteResult{}, err
	}
	id := p.ID
	if n == 0 {
		// Another call with the same request id won.
		row, err := dbq.New(s.Pool).GetIssueWriteByKey(ctx, p.IdempotencyKey)
		if err != nil {
			return WriteResult{}, err
		}
		if row.PayloadHash != p.PayloadHash {
			return WriteResult{}, ErrConflict
		}
		id = row.ID
	}
	if err := dbq.New(s.Pool).NotifyEvent(ctx, dbq.NotifyEventParams{Channel: Channel, Payload: id}); err != nil {
		s.log().WarnContext(ctx, "could not wake the issue write loop", "error", err)
	}
	return s.settle(ctx, id)
}

// settle waits for the row to reach a final state, up to Settle, and returns what it has.
func (s *Service) settle(ctx context.Context, id string) (WriteResult, error) {
	q := dbq.New(s.Pool)
	deadline := s.now().Add(s.Settle)
	for {
		row, err := q.GetIssueWrite(ctx, id)
		if err != nil {
			return WriteResult{}, err
		}
		if Final(row.State) || !s.now().Before(deadline) {
			return writeResultOf(row), nil
		}
		select {
		case <-ctx.Done():
			return writeResultOf(row), nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *Service) handle(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
	row, err := q.GetIssueWrite(ctx, c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), nil
	}
	if err != nil {
		return reconcile.Retry(), fmt.Errorf("load issue write: %w", err)
	}
	repo, ok := repoOf(row.RepoFullName)
	if !ok {
		return s.refuse(ctx, c, "invalid_repository")
	}
	if dbstate.IssueWriteState(c.State) != dbstate.IssueWritePending {
		return s.settleUnknown(ctx, c, row, repo)
	}

	// pending: a write from before a restart may have reached GitHub already, so look first.
	found, err := s.find(ctx, repo, row)
	if err != nil {
		return reconcile.Retry(), err
	}
	if found != nil {
		return s.created(ctx, c, *found)
	}
	// Nothing has been sent yet, so a read that fails is retried and a mismatch is a refusal.
	if why, err := s.stillTrue(ctx, repo, row); err != nil {
		return reconcile.Retry(), err
	} else if why != "" {
		return s.refuse(ctx, c, why)
	}
	owner, attempts := c.Owner, attemptsOf(c)
	n, err := q.MarkIssueWriteDispatching(ctx, dbq.MarkIssueWriteDispatchingParams{ID: c.ID, LeaseOwner: &owner, Attempts: attempts})
	if err != nil {
		return reconcile.Retry(), err
	}
	if n == 0 {
		return reconcile.Recorded(), reconcile.ErrLeaseLost
	}

	// From here the POST may run, so every exit but a rate limit leaves the row for a lookup.
	var w *github.IssueWrite
	if row.Kind == dbstate.IssueWriteComment {
		w, err = s.GitHub.CommentIssue(ctx, repo, int(deref64(row.IssueNumber)), row.Body)
	} else {
		w, err = s.GitHub.CreateIssue(ctx, repo, row.Title, row.Body, row.Labels)
	}
	switch st, hasStatus := github.StatusOf(err); {
	case err == nil && w != nil:
		return s.created(ctx, c, *w)
	case hasStatus && st == 429:
		if n, perr := q.MarkIssueWritePending(ctx, dbq.MarkIssueWritePendingParams{ID: c.ID, LeaseOwner: &owner, Attempts: attempts, Detail: ptr("rate limited")}); perr != nil || n == 0 {
			return reconcile.Recorded(), orLost(perr)
		}
		return reconcile.RetryAfter(s.ceiling()), nil
	case hasStatus && st >= 400 && st < 500:
		return s.refuse(ctx, c, fmt.Sprintf("github_%d", st))
	default:
		return s.uncertain(ctx, c, "the POST's outcome is not known")
	}
}

// stillTrue re-reads, just before the POST, what the call checked: the repository's identity and,
// for a comment, that the issue is still an issue. It returns the refusal code for a definite
// mismatch, empty when all still hold.
func (s *Service) stillTrue(ctx context.Context, repo github.Repo, row dbq.IssueWrite) (string, error) {
	detail, err := s.GitHub.RepoDetail(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("read repository: %w", err)
	}
	if detail.ID != row.RepoID || !strings.EqualFold(detail.FullName, row.RepoFullName) {
		return "repository_mismatch", nil
	}
	if row.Kind == dbstate.IssueWriteComment {
		_, err := s.GitHub.GetIssue(ctx, repo, int(deref64(row.IssueNumber)))
		switch {
		case errors.Is(err, github.ErrNotFound):
			return "issue_missing", nil
		case err != nil:
			return "", fmt.Errorf("read issue: %w", err)
		}
	}
	return "", nil
}

// find looks for the write on GitHub by its marker: an issue created, or a comment on the issue,
// since shortly before the intent was written, by a bot (the App), whose body carries the row's
// marker.
func (s *Service) find(ctx context.Context, repo github.Repo, row dbq.IssueWrite) (*github.IssueWrite, error) {
	since := row.CreatedAt.Add(-lookback)
	var (
		list []github.IssueWrite
		err  error
	)
	if row.Kind == dbstate.IssueWriteComment {
		list, err = s.GitHub.IssueCommentsSince(ctx, repo, int(deref64(row.IssueNumber)), since)
		if errors.Is(err, github.ErrNotFound) {
			// No issue, so no comment on it: stillTrue refuses a pending write, and an uncertain
			// one runs out its window.
			return nil, nil
		}
	} else {
		list, err = s.GitHub.IssuesCreatedSince(ctx, repo, since)
	}
	if err != nil {
		return nil, fmt.Errorf("look for the write: %w", err)
	}
	mark := marker(row.ID)
	for i := range list {
		if list[i].UserType == "Bot" && strings.Contains(list[i].Body, mark) {
			return &list[i], nil
		}
	}
	return nil, nil
}

// settleUnknown resolves a write whose POST may have run: it only ever looks.
func (s *Service) settleUnknown(ctx context.Context, c reconcile.Claim, row dbq.IssueWrite, repo github.Repo) (reconcile.Outcome, error) {
	found, err := s.find(ctx, repo, row)
	if err != nil {
		return reconcile.Retry(), err
	}
	if found != nil {
		return s.created(ctx, c, *found)
	}
	owner := c.Owner
	m, err := dbq.New(s.Pool).MarkIssueWriteUncertain(ctx, dbq.MarkIssueWriteUncertainParams{
		ID: c.ID, LeaseOwner: &owner, Attempts: attemptsOf(c), Detail: ptr("not found on GitHub yet"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), reconcile.ErrLeaseLost
	}
	if err != nil {
		return reconcile.Retry(), err
	}
	if s.now().Sub(m.UpdatedAt) >= s.window() {
		return s.refuse(ctx, c, "not_created")
	}
	return reconcile.RetryAfter(s.recheckDelay(int(m.Rechecks))), nil
}

func (s *Service) uncertain(ctx context.Context, c reconcile.Claim, why string) (reconcile.Outcome, error) {
	owner := c.Owner
	m, err := dbq.New(s.Pool).MarkIssueWriteUncertain(ctx, dbq.MarkIssueWriteUncertainParams{
		ID: c.ID, LeaseOwner: &owner, Attempts: attemptsOf(c), Detail: &why,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), reconcile.ErrLeaseLost
	}
	if err != nil {
		return reconcile.Retry(), err
	}
	s.log().Warn("issue write uncertain", "write", c.ID, "detail", why)
	return reconcile.RetryAfter(s.recheckDelay(int(m.Rechecks))), nil
}

func (s *Service) created(ctx context.Context, c reconcile.Claim, w github.IssueWrite) (reconcile.Outcome, error) {
	owner := c.Owner
	var comment *int64
	if w.CommentID > 0 {
		comment = &w.CommentID
	}
	n, err := dbq.New(s.Pool).FinishIssueWriteCreated(ctx, dbq.FinishIssueWriteCreatedParams{
		ID: c.ID, LeaseOwner: &owner, Attempts: attemptsOf(c),
		IssueNumber: ptr(int64(w.Number)), CommentID: comment, Url: ptr(w.HTMLURL),
	})
	if err != nil {
		return reconcile.Retry(), err
	}
	if n == 0 {
		return reconcile.Recorded(), reconcile.ErrLeaseLost
	}
	s.log().Info("issue write created", "write", c.ID, "issue", w.Number, "comment", w.CommentID)
	return reconcile.Recorded(), nil
}

func (s *Service) refuse(ctx context.Context, c reconcile.Claim, why string) (reconcile.Outcome, error) {
	owner := c.Owner
	n, err := dbq.New(s.Pool).FinishIssueWriteRefused(ctx, dbq.FinishIssueWriteRefusedParams{
		ID: c.ID, LeaseOwner: &owner, Attempts: attemptsOf(c), Detail: &why,
	})
	if err != nil {
		return reconcile.Retry(), err
	}
	if n == 0 {
		return reconcile.Recorded(), reconcile.ErrLeaseLost
	}
	s.log().Warn("issue write refused", "write", c.ID, "reason", why)
	return reconcile.Recorded(), nil
}

func (s *Service) recheckDelay(n int) time.Duration {
	d := orDefault(s.Recheck, 2*time.Second)
	for range min(n, 10) {
		d *= 2
		if d >= s.ceiling() {
			return s.ceiling()
		}
	}
	return min(d, s.ceiling())
}

func (s *Service) ceiling() time.Duration { return orDefault(s.Ceiling, 30*time.Second) }

func (s *Service) window() time.Duration { return orDefault(s.Window, 10*time.Minute) }

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func ptr[T any](v T) *T { return &v }

func deref64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func orLost(err error) error {
	if err != nil {
		return err
	}
	return reconcile.ErrLeaseLost
}

// attemptsOf is the claim's attempt counter as the fenced queries take it.
func attemptsOf(c reconcile.Claim) int32 {
	return int32(c.Attempts) //nolint:gosec // a small counter
}
