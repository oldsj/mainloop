// Package projects imports GitHub repositories as projects and serves their list, detail and
// merge policy. Mainloop owns the project row; GitHub is read through an App-scoped client and
// its failures never fail a read of Mainloop's own state.
package projects

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/gitref"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Typed failures the API layer maps to the contract's statuses.
var (
	ErrNotFound        = errors.New("projects: project not found")
	ErrInvalidRepo     = errors.New("projects: invalid GitHub repository")
	ErrInvalidPolicy   = errors.New("projects: unknown merge policy")
	ErrWritesDisabled  = errors.New("projects: owner policy writes are disabled")
	ErrVersionConflict = errors.New("projects: merge policy version changed")
)

// Remote is the part of the GitHub client the projects use.
type Remote interface {
	Metadata(ctx context.Context, repo github.Repo) (*github.Metadata, error)
	OpenPullRequests(ctx context.Context, repo github.Repo, limit int) ([]github.PullRequest, error)
	RecentCommits(ctx context.Context, repo github.Repo, branch string, limit int) ([]github.Commit, error)
}

// PolicyAdvancer records a changed default branch in the project's push policy. The push gate
// implements it; nil means no push gate runs here and there is no policy to advance.
type PolicyAdvancer interface {
	AdvanceDefaultBranch(ctx context.Context, projectID, branch string) error
}

// Sweep tuning: how often the sweep looks, how old metadata may get before it is read again, how
// long a failing project is left alone, and how many projects one run reads.
const (
	metadataSweepEvery  = time.Minute
	metadataStaleAfter  = 15 * time.Minute
	metadataRetryAfter  = 5 * time.Minute
	metadataSweepBatch  = 25
	metadataSweepWindow = 200
)

// errRepoMismatch means GitHub answered for a different repository than the one asked about.
var errRepoMismatch = errors.New("projects: GitHub answered for another repository")

// defaultMetadataTimeout bounds the whole GitHub fetch of an import. A healthy import is four
// short calls (installation, token, repository, pull requests) and finishes in a second or two;
// ten seconds leaves room for a slow GitHub while the owner is still waiting on the response, and
// keeps the worst case far under the 4 x 15 s the per-request limits would allow.
const defaultMetadataTimeout = 10 * time.Second

// detailLimit is how many pull requests and commits the detail view shows.
const detailLimit = 10

// Service holds the project operations.
type Service struct {
	Pool *pgxpool.Pool
	// Remote reads GitHub. Nil means no GitHub App is configured: import still records the
	// project and detail shows no pull requests or commits.
	Remote Remote
	// OwnerID is the single configured owner every project belongs to.
	OwnerID string
	// PolicyWrites is MAINLOOP_OWNER_POLICY_WRITES_ENABLED.
	PolicyWrites bool
	// MetadataTimeout bounds the best-effort GitHub fetch during import; zero means
	// [defaultMetadataTimeout].
	MetadataTimeout time.Duration
	// Policy advances the push policy when GitHub's default branch changes. Nil skips that step.
	Policy PolicyAdvancer
	// Now is the clock for staleness and retry decisions; nil means time.Now.
	Now func() time.Time
	Log *slog.Logger
	// NeedsOwner reports whether a session is waiting on the owner. The attention projection
	// owns that answer; until it is wired every session reads false.
	NeedsOwner func(ctx context.Context, sessionID string) bool
}

// Import records the owner's repository as a project. The row is written first; the GitHub
// metadata fetch that follows is best effort, so a GitHub outage or a missing installation leaves
// a usable project whose metadata is filled in by a later import.
func (s *Service) Import(ctx context.Context, text string) (dbq.Project, error) {
	repo, err := github.ParseRepo(text)
	if err != nil {
		return dbq.Project{}, ErrInvalidRepo
	}
	p, err := dbq.New(s.Pool).ImportProject(ctx, dbq.ImportProjectParams{
		ID: uuid.NewString(), UserID: s.OwnerID, Owner: repo.Owner, Name: repo.Name,
		FullName: repo.FullName(), DefaultBranch: "", HTMLURL: repo.HTMLURL(),
	})
	if err != nil {
		return dbq.Project{}, fmt.Errorf("import project: %w", err)
	}
	if s.Remote == nil {
		return p, nil
	}
	refreshed, err := s.refresh(ctx, p)
	switch {
	case err == nil, errors.Is(err, errMetadataMoved):
		return refreshed, nil // a moved row is the fresher one another refresh stored
	case errors.Is(err, errRepoMismatch):
		return p, nil
	default:
		var w *writeError
		if errors.As(err, &w) {
			return dbq.Project{}, err
		}
		s.Log.WarnContext(ctx, "project metadata unavailable", "project_id", p.ID, "error", err.Error())
		return p, nil
	}
}

// writeError marks a failure of Mainloop's own database, as opposed to GitHub or the push gate.
type writeError struct{ err error }

func (e *writeError) Error() string { return e.err.Error() }
func (e *writeError) Unwrap() error { return e.err }

// errMetadataMoved means another refresh stored newer metadata while this one waited on GitHub.
var errMetadataMoved = errors.New("projects: metadata refreshed by another caller")

// refresh reads the project's repository from GitHub and stores the answer. The network read
// happens first and alone; nothing is written when GitHub cannot be read, answers for another
// repository or omits the default branch or URL, so a failure leaves the row as it was.
//
// The push policy follows GitHub's default branch on every successful refresh, not only when it
// differs from the project row: the advance does nothing when the policy already names the branch,
// and a policy that lags the row (a gate-off period, an older build) is repaired by the next pass.
// It advances before the project row does, so a failure between the two leaves policy and project
// disagreeing, which the push gate refuses, and the next refresh finishes the job.
//
// A refresh whose project row was refreshed by someone else during the GitHub read stops with
// errMetadataMoved before touching the policy, and the row write is conditional on the same
// check, so an older answer never overwrites a newer one.
func (s *Service) refresh(ctx context.Context, p dbq.Project) (dbq.Project, error) {
	repo := github.Repo{Owner: p.Owner, Name: p.Name}
	timeout := s.MetadataTimeout
	if timeout <= 0 {
		timeout = defaultMetadataTimeout
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	md, err := s.Remote.Metadata(fetchCtx, repo)
	if err != nil {
		return p, fmt.Errorf("read GitHub metadata: %w", err)
	}
	if !md.Repo.Same(repo) {
		return p, errRepoMismatch
	}
	if !gitref.Valid(md.DefaultBranch) {
		return p, errors.New("GitHub returned no usable default branch")
	}
	if md.HTMLURL == "" {
		return p, errors.New("GitHub returned no repository URL")
	}
	q := dbq.New(s.Pool)
	if s.Policy != nil {
		now, err := q.GetOwnedProject(ctx, dbq.GetOwnedProjectParams{ID: p.ID, UserID: s.OwnerID})
		if err != nil {
			return p, &writeError{fmt.Errorf("re-read project: %w", err)}
		}
		if !sameInstant(now.MetadataUpdatedAt, p.MetadataUpdatedAt) {
			return now, errMetadataMoved
		}
		if err := s.Policy.AdvanceDefaultBranch(ctx, p.ID, md.DefaultBranch); err != nil {
			return p, fmt.Errorf("advance push policy: %w", err)
		}
	}
	refreshed, err := q.RefreshProjectMetadata(ctx, dbq.RefreshProjectMetadataParams{
		ID: p.ID, UserID: s.OwnerID, Owner: md.Repo.Owner, Name: md.Repo.Name, FullName: md.Repo.FullName(),
		HTMLURL: md.HTMLURL, Description: md.Description, DefaultBranch: md.DefaultBranch,
		AvatarURL: md.AvatarURL, OpenPRCount: count32(md.OpenPRs),
		OpenIssueCount:        count32(md.OpenIssues),
		ReadMetadataUpdatedAt: p.MetadataUpdatedAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		now, getErr := q.GetOwnedProject(ctx, dbq.GetOwnedProjectParams{ID: p.ID, UserID: s.OwnerID})
		if getErr != nil {
			return p, &writeError{fmt.Errorf("re-read project: %w", getErr)}
		}
		return now, errMetadataMoved
	}
	if err != nil {
		return p, &writeError{fmt.Errorf("refresh project metadata: %w", err)}
	}
	return refreshed, nil
}

// sameInstant reports whether two optional timestamps are the same moment.
func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// MetadataSweep keeps project metadata current. It reads projects whose default branch or URL is
// empty, or whose metadata is older than a quarter hour, from GitHub, so the push gate's check
// that GitHub and the project agree on the default branch can pass and a renamed default branch
// reaches the push policy. It replaces an owner-triggered refresh route. Each sweep value keeps
// its own memory of recent failures.
func (s *Service) MetadataSweep() reconcile.Sweep {
	failures := &failures{at: map[string]time.Time{}}
	return reconcile.Sweep{Name: "project-metadata", Every: metadataSweepEvery, Run: func(ctx context.Context) error {
		return s.refreshStale(ctx, failures)
	}}
}

// failures remembers when a project's refresh last failed so that one project GitHub cannot
// answer for (no installation, an outage) is not asked about every minute.
type failures struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (f *failures) due(id string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.at[id]
	return !ok || now.Sub(at) >= metadataRetryAfter
}

func (f *failures) record(id string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.at[id] = now
}

func (f *failures) clear(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.at, id)
}

// refreshStale is one pass of the sweep. A GitHub or push-policy failure on one project is logged
// and leaves it unchanged; it is tried again after a pause, and the other projects are still
// refreshed. Only an error of Mainloop's own database fails the pass.
func (s *Service) refreshStale(ctx context.Context, f *failures) error {
	if s.Remote == nil {
		return nil
	}
	now := s.now()
	before := now.Add(-metadataStaleAfter)
	rows, err := dbq.New(s.Pool).ListProjectsNeedingMetadata(ctx, dbq.ListProjectsNeedingMetadataParams{
		UserID: s.OwnerID, MetadataUpdatedAt: &before, Limit: metadataSweepWindow,
	})
	if err != nil {
		return fmt.Errorf("list projects needing metadata: %w", err)
	}
	attempts := 0
	for _, p := range rows {
		if attempts >= metadataSweepBatch {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !f.due(p.ID, now) {
			continue
		}
		attempts++
		_, err := s.refresh(ctx, p)
		if err == nil || errors.Is(err, errMetadataMoved) {
			f.clear(p.ID) // another refresh got there first, which is as good
			continue
		}
		var w *writeError
		if errors.As(err, &w) {
			return err
		}
		f.record(p.ID, now)
		s.Log.WarnContext(ctx, "project metadata refresh failed", "project_id", p.ID, "error", err.Error())
	}
	return nil
}

// count32 fits a GitHub count into the integer column.
func count32(n int) int32 {
	if n < 0 {
		return 0
	}
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}

// List returns the owner's projects, most recently used first.
func (s *Service) List(ctx context.Context, limit int32) ([]dbq.Project, error) {
	if limit < 1 {
		return []dbq.Project{}, nil
	}
	ps, err := dbq.New(s.Pool).ListOwnedProjects(ctx, dbq.ListOwnedProjectsParams{UserID: s.OwnerID, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return ps, nil
}

// Get returns one of the owner's projects.
func (s *Service) Get(ctx context.Context, id string) (dbq.Project, error) {
	p, err := dbq.New(s.Pool).GetOwnedProject(ctx, dbq.GetOwnedProjectParams{ID: id, UserID: s.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return dbq.Project{}, ErrNotFound
	}
	if err != nil {
		return dbq.Project{}, fmt.Errorf("get project: %w", err)
	}
	return p, nil
}

// Detail is a project with its live GitHub data and its sessions.
type Detail struct {
	Project  dbq.Project
	PRs      []github.PullRequest
	Commits  []github.Commit
	Sessions []dbq.ListProjectSessionsRow
	// NeedsOwner holds the session ids that wait on the owner.
	NeedsOwner map[string]bool
}

// Detail reads the project, then asks GitHub for its open pull requests and recent commits. A
// GitHub failure shows as an empty list: the contract has no error for it, and the project and
// its sessions are still true.
func (s *Service) Detail(ctx context.Context, id string) (*Detail, error) {
	p, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &Detail{Project: p, PRs: []github.PullRequest{}, Commits: []github.Commit{}, NeedsOwner: map[string]bool{}}
	if s.Remote != nil {
		repo := github.Repo{Owner: p.Owner, Name: p.Name}
		var wg sync.WaitGroup
		wg.Go(func() {
			prs, err := s.Remote.OpenPullRequests(ctx, repo, detailLimit)
			if err != nil {
				s.Log.WarnContext(ctx, "project pull requests unavailable", "project_id", p.ID, "error", err.Error())
				return
			}
			d.PRs = prs
		})
		wg.Go(func() {
			commits, err := s.Remote.RecentCommits(ctx, repo, p.DefaultBranch, detailLimit)
			if err != nil {
				s.Log.WarnContext(ctx, "project commits unavailable", "project_id", p.ID, "error", err.Error())
				return
			}
			d.Commits = commits
		})
		wg.Wait()
	}
	d.Sessions, err = dbq.New(s.Pool).ListProjectSessions(ctx, dbq.ListProjectSessionsParams{UserID: s.OwnerID, ProjectID: &id})
	if err != nil {
		return nil, fmt.Errorf("list project sessions: %w", err)
	}
	if s.NeedsOwner != nil {
		for _, sess := range d.Sessions {
			if s.NeedsOwner(ctx, sess.ID) {
				d.NeedsOwner[sess.ID] = true
			}
		}
	}
	return d, nil
}

// MergePolicy is a project's policy and its version.
type MergePolicy struct {
	Policy  dbstate.MergePolicy
	Version int64
}

// MergePolicy reads a project's merge policy.
func (s *Service) MergePolicy(ctx context.Context, id string) (MergePolicy, error) {
	p, err := s.Get(ctx, id)
	if err != nil {
		return MergePolicy{}, err
	}
	return MergePolicy{Policy: p.MergePolicy, Version: p.MergePolicyVersion}, nil
}

// SetMergePolicy changes a project's merge policy if its version is still expected. The check,
// the change and the audit row are one transaction holding the project row lock, so a merge claim
// that takes the same lock sees either the old policy or the new one. Writing the policy it
// already has changes nothing and does not bump the version.
func (s *Service) SetMergePolicy(ctx context.Context, id string, policy dbstate.MergePolicy, expected int64) (MergePolicy, error) {
	if !s.PolicyWrites {
		return MergePolicy{}, ErrWritesDisabled
	}
	if policy != dbstate.MergeAuto && policy != dbstate.MergeApproval {
		return MergePolicy{}, ErrInvalidPolicy
	}
	var out MergePolicy
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		p, err := q.LockOwnedProject(ctx, dbq.LockOwnedProjectParams{ID: id, UserID: s.OwnerID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if p.MergePolicyVersion != expected {
			return ErrVersionConflict
		}
		if p.MergePolicy == policy {
			out = MergePolicy{Policy: p.MergePolicy, Version: p.MergePolicyVersion}
			return nil
		}
		changed, err := q.SetOwnedProjectMergePolicy(ctx, dbq.SetOwnedProjectMergePolicyParams{ID: id, MergePolicy: policy})
		if err != nil {
			return err
		}
		if err := q.InsertMergePolicyAudit(ctx, dbq.InsertMergePolicyAuditParams{
			ProjectID: id, OwnerID: s.OwnerID, OldPolicy: string(p.MergePolicy), NewPolicy: string(policy),
			Version: changed.MergePolicyVersion,
		}); err != nil {
			return err
		}
		out = MergePolicy{Policy: changed.MergePolicy, Version: changed.MergePolicyVersion}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrVersionConflict) {
			return MergePolicy{}, err
		}
		return MergePolicy{}, fmt.Errorf("set merge policy: %w", err)
	}
	return out, nil
}

// NewService builds the service. remote is nil when no GitHub App is configured. policy advances the
// push policy when GitHub's default branch changes. needsOwner reports whether a session is waiting
// on the owner (the attention projection owns that answer). Nil for a test that needs none.
func NewService(cfg *config.Config, pool *pgxpool.Pool, remote Remote, policy PolicyAdvancer,
	needsOwner func(ctx context.Context, sessionID string) bool, log *slog.Logger) *Service {
	return &Service{
		Pool: pool, Remote: remote, OwnerID: cfg.Identity.OwnerID,
		PolicyWrites: bool(cfg.OwnerPolicyWritesEnabled), Policy: policy, NeedsOwner: needsOwner, Log: log,
	}
}
