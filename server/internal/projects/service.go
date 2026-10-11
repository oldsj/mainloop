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

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
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
	Log             *slog.Logger
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
	timeout := s.MetadataTimeout
	if timeout <= 0 {
		timeout = defaultMetadataTimeout
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	md, err := s.Remote.Metadata(fetchCtx, repo)
	if err != nil {
		s.Log.WarnContext(ctx, "project metadata unavailable", "project_id", p.ID, "error", err.Error())
		return p, nil
	}
	if !md.Repo.Same(repo) {
		return p, nil
	}
	refreshed, err := dbq.New(s.Pool).RefreshProjectMetadata(ctx, dbq.RefreshProjectMetadataParams{
		ID: p.ID, UserID: s.OwnerID, Owner: md.Repo.Owner, Name: md.Repo.Name, FullName: md.Repo.FullName(),
		HTMLURL: md.HTMLURL, Description: md.Description, DefaultBranch: md.DefaultBranch,
		AvatarURL: md.AvatarURL, OpenPRCount: count32(md.OpenPRs),
		OpenIssueCount: count32(md.OpenIssues),
	})
	if err != nil {
		return dbq.Project{}, fmt.Errorf("refresh project metadata: %w", err)
	}
	return refreshed, nil
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
