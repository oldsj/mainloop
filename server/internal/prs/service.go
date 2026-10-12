package prs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Channel wakes the creation loop when a row is written.
const Channel = "mainloop_pr_creations"

// GitHub is the part of the GitHub App the pull-request calls use.
type GitHub interface {
	RepoDetail(ctx context.Context, repo github.Repo) (*github.RepoDetail, error)
	BranchTip(ctx context.Context, repo github.Repo, branch string) (string, error)
	PullRequestsForHead(ctx context.Context, repo github.Repo, head, base string) ([]github.PullRequestInfo, error)
	CreatePullRequest(ctx context.Context, repo github.Repo, head, base, title, body string) (*github.PullRequestInfo, error)
}

// Errors a caller shows to the agent.
var (
	// ErrNotFound means the project is not the owner's.
	ErrNotFound = errors.New("prs: project not found")
	// ErrConflict means the request id was used with other arguments.
	ErrConflict = errors.New("prs: request id reused with different arguments")
)

// Refusal is a request Mainloop refused before writing anything. Code is stable.
type Refusal struct{ Code, Message string }

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// Service opens pull requests.
type Service struct {
	Pool   *pgxpool.Pool
	GitHub GitHub
	Log    *slog.Logger

	// Settle is how long Open waits for the loop to settle the row before it returns the state it
	// has (default 20s). A zero Settle returns at once.
	Settle time.Duration
	// Window is how long a creation whose POST may have run keeps looking for the pull request
	// before it concludes none was made (default 10m).
	Window time.Duration
	// Recheck and Ceiling pace the lookups of an unresolved creation (defaults 2s and 30s).
	Recheck, Ceiling time.Duration
	Lease            time.Duration
	Now              func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) window() time.Duration { return orDefault(s.Window, 10*time.Minute) }

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Table is the reconcile table of creations.
func Table() reconcile.Table {
	return reconcile.NewTable("pr_creations",
		string(dbstate.PRPending), string(dbstate.PRDispatching), string(dbstate.PRUncertain))
}

// Loop carries creations out.
func (s *Service) Loop() reconcile.Loop {
	return reconcile.Loop{
		Name: "pr-creations", Table: Table(), Handle: s.handle, Lease: s.Lease, Wake: []string{Channel},
	}
}

// OpenInput is one open_pull_request call.
type OpenInput struct {
	OwnerID, ProjectID, SessionID               string
	Branch, ExpectedSHA, Title, Body, RequestID string
}

// Result is the state of a creation as the agent sees it.
type Result struct {
	ID         string
	State      dbstate.PRCreationState
	Repository string
	Head, Base string
	Number     int64
	URL        string
	Detail     string
}

func resultOf(r dbq.PrCreation) Result {
	out := Result{ID: r.ID, State: r.State, Repository: r.RepoFullName, Head: r.Head, Base: r.Base}
	if r.PrNumber != nil {
		out.Number = *r.PrNumber
	}
	if r.Url != nil {
		out.URL = *r.Url
	}
	if r.Detail != nil {
		out.Detail = *r.Detail
	}
	return out
}

func payloadHash(in OpenInput) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]string{
		"project_id": in.ProjectID, "branch": in.Branch, "expected_sha": in.ExpectedSHA,
		"title": in.Title, "body": in.Body,
	})
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:])
}

func repoOf(fullName string) (github.Repo, bool) {
	owner, name, ok := strings.Cut(fullName, "/")
	return github.Repo{Owner: owner, Name: name}, ok && owner != "" && name != ""
}

// Open records the intent to open a pull request and waits briefly for the loop to settle it. A
// replay of the same request id returns the same creation; the same request id with other arguments
// is [ErrConflict].
func (s *Service) Open(ctx context.Context, in OpenInput) (Result, error) {
	q := dbq.New(s.Pool)
	key := "pr:" + in.OwnerID + ":" + in.RequestID
	hash := payloadHash(in)

	// A replay needs no GitHub call.
	row, err := q.GetPRCreationByKey(ctx, key)
	switch {
	case err == nil:
		if row.PayloadHash != hash {
			return Result{}, ErrConflict
		}
		return s.settle(ctx, row.ID)
	case !errors.Is(err, pgx.ErrNoRows):
		return Result{}, err
	}

	project, err := q.GetProjectAuthority(ctx, dbq.GetProjectAuthorityParams{ID: in.ProjectID, UserID: in.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrNotFound
	}
	if err != nil {
		return Result{}, err
	}
	repo, ok := repoOf(project.FullName)
	if !ok {
		return Result{}, &Refusal{"repository_mismatch", "the project has no usable repository name"}
	}
	detail, err := s.GitHub.RepoDetail(ctx, repo)
	if err != nil {
		return Result{}, &Refusal{"github_unavailable", "GitHub could not be read: " + statusText(err)}
	}
	if !strings.EqualFold(detail.FullName, project.FullName) {
		return Result{}, &Refusal{"repository_mismatch", "the repository is not the project's"}
	}
	base := detail.DefaultBranch
	if in.Branch == base {
		return Result{}, &Refusal{"default_branch", "the head branch is the default branch"}
	}
	tip, err := s.GitHub.BranchTip(ctx, repo, in.Branch)
	switch {
	case errors.Is(err, github.ErrBranchName):
		return Result{}, &Refusal{"invalid_branch", "the branch name is not supported"}
	case errors.Is(err, github.ErrNotFound):
		return Result{}, &Refusal{"branch_missing", "the branch does not exist on GitHub"}
	case err != nil:
		return Result{}, &Refusal{"github_unavailable", "GitHub could not be read: " + statusText(err)}
	}
	if tip != in.ExpectedSHA {
		return Result{}, &Refusal{"branch_moved", "the branch tip is not expected_sha"}
	}

	id := uuid.NewString()
	var sess *string
	if in.SessionID != "" {
		sess = &in.SessionID
	}
	// The intent is the tool call's write, so it runs in a guarded transaction (the MCP credential is
	// re-checked in it); the GitHub reads above stay outside any transaction.
	var n int64
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		var ierr error
		n, ierr = dbq.New(tx).InsertPRCreation(ctx, dbq.InsertPRCreationParams{
			ID: id, IdempotencyKey: key, OwnerID: in.OwnerID, ProjectID: in.ProjectID, SessionID: sess,
			RequestID: in.RequestID, PayloadHash: hash, RepoID: detail.ID, RepoFullName: detail.FullName,
			Head: in.Branch, Base: base, ExpectedSha: in.ExpectedSHA, Title: in.Title, Body: in.Body,
		})
		return ierr
	})
	if err != nil {
		return Result{}, err
	}
	if n == 0 {
		// Another call won: the same request id, or another request for the same head.
		row, err = q.GetPRCreationByKey(ctx, key)
		if errors.Is(err, pgx.ErrNoRows) {
			row, err = q.GetPRCreationByHead(ctx, dbq.GetPRCreationByHeadParams{
				OwnerID: in.OwnerID, RepoID: detail.ID, Head: in.Branch, Base: base,
			})
		}
		if err != nil {
			return Result{}, err
		}
		if row.IdempotencyKey == key && row.PayloadHash != hash {
			return Result{}, ErrConflict
		}
		id = row.ID
	}
	if err := q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: Channel, Payload: id}); err != nil {
		s.log().WarnContext(ctx, "could not wake the creation loop", "error", err)
	}
	return s.settle(ctx, id)
}

// settle waits for the row to reach a final state, up to Settle, and returns what it has.
func (s *Service) settle(ctx context.Context, id string) (Result, error) {
	q := dbq.New(s.Pool)
	deadline := s.now().Add(s.Settle)
	for {
		row, err := q.GetPRCreation(ctx, id)
		if err != nil {
			return Result{}, err
		}
		if Final(row.State) || !s.now().Before(deadline) {
			return resultOf(row), nil
		}
		select {
		case <-ctx.Done():
			return resultOf(row), nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Get returns a creation of the owner's.
func (s *Service) Get(ctx context.Context, ownerID, id string) (Result, error) {
	row, err := dbq.New(s.Pool).GetPRCreation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.OwnerID != ownerID) {
		return Result{}, ErrNotFound
	}
	if err != nil {
		return Result{}, err
	}
	return resultOf(row), nil
}

func statusText(err error) string {
	if st, ok := github.StatusOf(err); ok {
		return fmt.Sprintf("status %d", st)
	}
	return "unreachable"
}

// matches reports whether a listed or created pull request is the one this creation asked for:
// the same repository, head branch, base branch and head commit.
func matches(row dbq.PrCreation, pr github.PullRequestInfo) bool {
	return pr.Number > 0 && pr.Head.Ref == row.Head && pr.Base.Ref == row.Base && pr.Head.SHA == row.ExpectedSha &&
		pr.Head.RepoID == row.RepoID && pr.Base.RepoID == row.RepoID &&
		strings.EqualFold(pr.Head.RepoFullName, row.RepoFullName) && strings.EqualFold(pr.Base.RepoFullName, row.RepoFullName)
}

func (s *Service) handle(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
	row, err := q.GetPRCreation(ctx, c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), nil
	}
	if err != nil {
		return reconcile.Retry(), fmt.Errorf("load creation: %w", err)
	}
	repo, ok := repoOf(row.RepoFullName)
	if !ok {
		return s.refuse(ctx, c, "invalid_repository")
	}
	owner := c.Owner
	attempts := attemptsOf(c)

	if dbstate.PRCreationState(c.State) != dbstate.PRPending {
		return s.settleUnknown(ctx, c, row, repo)
	}

	// pending: the pull request may exist already (a creation from before a restart that never
	// reached the POST, or one made by hand), so look before the one POST.
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
	if err := Transition(dbstate.PRPending, dbstate.PRDispatching); err != nil {
		return reconcile.Retry(), err
	}
	n, err := q.MarkPRDispatching(ctx, dbq.MarkPRDispatchingParams{ID: c.ID, LeaseOwner: &owner, Attempts: attempts})
	if err != nil {
		return reconcile.Retry(), err
	}
	if n == 0 {
		return reconcile.Recorded(), reconcile.ErrLeaseLost
	}

	// From here the POST may run, so every exit but a rate limit leaves the row for a lookup.
	pr, err := s.GitHub.CreatePullRequest(ctx, repo, row.Head, row.Base, row.Title, row.Body)
	switch st, hasStatus := github.StatusOf(err); {
	case err == nil && pr != nil && matches(row, *pr):
		return s.created(ctx, c, *pr)
	case err == nil:
		return s.uncertain(ctx, c, "GitHub answered with a pull request that is not the one asked for")
	case hasStatus && st == 429:
		if n, perr := q.MarkPRPending(ctx, dbq.MarkPRPendingParams{ID: c.ID, LeaseOwner: &owner, Attempts: attempts, Detail: ptr("rate limited")}); perr != nil || n == 0 {
			return reconcile.Recorded(), orLost(perr)
		}
		return reconcile.RetryAfter(s.ceiling()), nil
	case hasStatus && st >= 400 && st < 500:
		return s.refuse(ctx, c, fmt.Sprintf("github_%d", st))
	default:
		return s.uncertain(ctx, c, "the POST's outcome is not known")
	}
}

// stillTrue re-reads what Open recorded just before the POST: the repository's identity, its default
// branch (the base) and the exact head commit. It returns the refusal code for a definite mismatch,
// empty when all still hold. The POST names only the branch, so a branch that moves after this read
// can still open a pull request on a newer head; that window is narrow and cannot be closed from
// here, and the merge path pins the head commit again before it merges.
func (s *Service) stillTrue(ctx context.Context, repo github.Repo, row dbq.PrCreation) (string, error) {
	detail, err := s.GitHub.RepoDetail(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("read repository: %w", err)
	}
	if detail.ID != row.RepoID || !strings.EqualFold(detail.FullName, row.RepoFullName) {
		return "repository_mismatch", nil
	}
	if detail.DefaultBranch != row.Base {
		return "default_branch_moved", nil
	}
	tip, err := s.GitHub.BranchTip(ctx, repo, row.Head)
	switch {
	case errors.Is(err, github.ErrNotFound), errors.Is(err, github.ErrBranchName):
		return "branch_missing", nil
	case err != nil:
		return "", fmt.Errorf("read branch: %w", err)
	}
	if tip != row.ExpectedSha {
		return "branch_moved", nil
	}
	return "", nil
}

func ptr[T any](v T) *T { return &v }

func orLost(err error) error {
	if err != nil {
		return err
	}
	return reconcile.ErrLeaseLost
}

func (s *Service) ceiling() time.Duration { return orDefault(s.Ceiling, 30*time.Second) }

// find lists the pull requests for the row's head and returns the oldest one that matches.
func (s *Service) find(ctx context.Context, repo github.Repo, row dbq.PrCreation) (*github.PullRequestInfo, error) {
	list, err := s.GitHub.PullRequestsForHead(ctx, repo, row.Head, row.Base)
	if err != nil {
		return nil, fmt.Errorf("list pull requests: %w", err)
	}
	var best *github.PullRequestInfo
	for i := range list {
		if matches(row, list[i]) && (best == nil || list[i].Number < best.Number) {
			best = &list[i]
		}
	}
	return best, nil
}

// settleUnknown resolves a creation whose POST may have run: it only ever looks.
func (s *Service) settleUnknown(ctx context.Context, c reconcile.Claim, row dbq.PrCreation, repo github.Repo) (reconcile.Outcome, error) {
	found, err := s.find(ctx, repo, row)
	if err != nil {
		return reconcile.Retry(), err
	}
	if found != nil {
		return s.created(ctx, c, *found)
	}
	q := dbq.New(s.Pool)
	owner := c.Owner
	m, err := q.MarkPRUncertain(ctx, dbq.MarkPRUncertainParams{
		ID: c.ID, LeaseOwner: &owner, Attempts: attemptsOf(c), Detail: ptr("no pull request found for the head yet"), //nolint:gosec // a small counter
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

func (s *Service) uncertain(ctx context.Context, c reconcile.Claim, why string) (reconcile.Outcome, error) {
	owner := c.Owner
	m, err := dbq.New(s.Pool).MarkPRUncertain(ctx, dbq.MarkPRUncertainParams{
		ID: c.ID, LeaseOwner: &owner, Attempts: attemptsOf(c), Detail: &why, //nolint:gosec // a small counter
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), reconcile.ErrLeaseLost
	}
	if err != nil {
		return reconcile.Retry(), err
	}
	s.log().Warn("pull request creation uncertain", "creation", c.ID, "detail", why)
	return reconcile.RetryAfter(s.recheckDelay(int(m.Rechecks))), nil
}

func (s *Service) created(ctx context.Context, c reconcile.Claim, pr github.PullRequestInfo) (reconcile.Outcome, error) {
	owner := c.Owner
	n, err := dbq.New(s.Pool).FinishPRCreated(ctx, dbq.FinishPRCreatedParams{
		ID: c.ID, LeaseOwner: &owner, Attempts: attemptsOf(c), PrNumber: ptr(int64(pr.Number)), Url: ptr(pr.HTMLURL),
	})
	if err != nil {
		return reconcile.Retry(), err
	}
	if n == 0 {
		return reconcile.Recorded(), reconcile.ErrLeaseLost
	}
	s.log().Info("pull request created", "creation", c.ID, "number", pr.Number)
	return reconcile.Recorded(), nil
}

func (s *Service) refuse(ctx context.Context, c reconcile.Claim, why string) (reconcile.Outcome, error) {
	if err := Transition(dbstate.PRCreationState(c.State), dbstate.PRRefused); err != nil {
		return reconcile.Retry(), err
	}
	owner := c.Owner
	n, err := dbq.New(s.Pool).FinishPRRefused(ctx, dbq.FinishPRRefusedParams{
		ID: c.ID, LeaseOwner: &owner, Attempts: attemptsOf(c), Detail: &why,
	})
	if err != nil {
		return reconcile.Retry(), err
	}
	if n == 0 {
		return reconcile.Recorded(), reconcile.ErrLeaseLost
	}
	s.log().Warn("pull request creation refused", "creation", c.ID, "reason", why)
	return reconcile.Recorded(), nil
}

// attemptsOf is the claim's attempt counter as the fenced queries take it.
func attemptsOf(c reconcile.Claim) int32 {
	return int32(c.Attempts) //nolint:gosec // a small counter
}

// DefaultSettle is how long a tool call waits for the loop to settle a creation before it answers
// with the state it has.
const DefaultSettle = 20 * time.Second

// NewService builds the service the processes run. The zero-valued Settle of a bare Service means
// "return at once", so the default is set here.
func NewService(pool *pgxpool.Pool, gh GitHub, log *slog.Logger) *Service {
	return &Service{Pool: pool, GitHub: gh, Log: log, Settle: DefaultSettle}
}
