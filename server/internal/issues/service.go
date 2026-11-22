// Package issues serves the GitHub issue, pull-request read and Dependabot-alert tools over the
// project's bound repository. The reads (this file and pulls.go) write nothing: a read that fails is simply retried by the
// caller. The writes (writes.go) follow the pull-request pattern: a call writes an intent row
// first, and a reconcile loop posts at most once and looks a lost answer up by a marker in the
// body, never resending.
package issues

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/github"
)

// GitHub is the part of the GitHub App the issue and alert tools use.
type GitHub interface {
	GetIssue(ctx context.Context, repo github.Repo, number int) (*github.Issue, error)
	ListDependabotAlerts(ctx context.Context, repo github.Repo) ([]github.DependabotAlert, error)
	RepoDetail(ctx context.Context, repo github.Repo) (*github.RepoDetail, error)
	LabelNames(ctx context.Context, repo github.Repo) ([]string, error)
	CreateIssue(ctx context.Context, repo github.Repo, title, body string, labels []string) (*github.IssueWrite, error)
	CommentIssue(ctx context.Context, repo github.Repo, number int, body string) (*github.IssueWrite, error)
	IssuesCreatedSince(ctx context.Context, repo github.Repo, since time.Time) ([]github.IssueWrite, error)
	IssueCommentsSince(ctx context.Context, repo github.Repo, number int, since time.Time) ([]github.IssueWrite, error)
	PullRequest(ctx context.Context, repo github.Repo, number int) (*github.PullRequestInfo, error)
	PullFiles(ctx context.Context, repo github.Repo, number, page, perPage int) ([]github.PullFile, bool, error)
	PullRequestFeedback(ctx context.Context, repo github.Repo, number int, since time.Time) ([]github.PRFeedback, error)
	PullCommentsSince(ctx context.Context, repo github.Repo, number int, since time.Time) ([]github.IssueWrite, error)
	CreateReview(ctx context.Context, repo github.Repo, number int, commitSHA, body string, comments []github.ReviewComment) (*github.IssueWrite, error)
	ReviewsSince(ctx context.Context, repo github.Repo, number int, since time.Time) ([]github.IssueWrite, error)
}

// ErrNotFound means the project is not the owner's.
var ErrNotFound = errors.New("issues: project not found")

// Refusal is a request Mainloop refused. Code is stable.
type Refusal struct{ Code, Message string }

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// Service reads issues and Dependabot alerts, and files issues and comments.
type Service struct {
	Pool   *pgxpool.Pool
	GitHub GitHub
	Log    *slog.Logger

	// Settle is how long a write call waits for the loop to settle its row before it returns the
	// state it has. A zero Settle returns at once; [NewService] sets [DefaultSettle].
	Settle time.Duration
	// Window is how long a write whose POST may have run keeps looking for it before it concludes
	// none was made (default 10m).
	Window time.Duration
	// Recheck and Ceiling pace the lookups of an unresolved write (defaults 2s and 30s).
	Recheck, Ceiling time.Duration
	Lease            time.Duration
	Now              func() time.Time
}

// DefaultSettle is how long a write call waits for the loop before it answers with the state it
// has.
const DefaultSettle = 20 * time.Second

// NewService builds the service the processes run: the MCP process's tools and the API process's
// write loop.
func NewService(pool *pgxpool.Pool, gh GitHub, log *slog.Logger) *Service {
	return &Service{Pool: pool, GitHub: gh, Log: log, Settle: DefaultSettle}
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func repoOf(fullName string) (github.Repo, bool) {
	owner, name, ok := strings.Cut(fullName, "/")
	return github.Repo{Owner: owner, Name: name}, ok && owner != "" && name != ""
}

// project resolves the owner's project to the GitHub repository it names.
func (s *Service) project(ctx context.Context, ownerID, projectID string) (github.Repo, error) {
	project, err := dbq.New(s.Pool).GetProjectAuthority(ctx, dbq.GetProjectAuthorityParams{ID: projectID, UserID: ownerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return github.Repo{}, ErrNotFound
	}
	if err != nil {
		return github.Repo{}, err
	}
	repo, ok := repoOf(project.FullName)
	if !ok {
		return github.Repo{}, &Refusal{"repository_mismatch", "the project has no usable repository name"}
	}
	return repo, nil
}

func statusText(err error) string {
	if st, ok := github.StatusOf(err); ok {
		return fmt.Sprintf("status %d", st)
	}
	return "unreachable"
}

// Issue is one issue as the agent sees it.
type Issue struct {
	Number  int
	Title   string
	Body    string
	State   string
	Labels  []string
	HTMLURL string
}

// GetIssue reads one issue of the project's repository.
func (s *Service) GetIssue(ctx context.Context, ownerID, projectID string, number int) (Issue, error) {
	repo, err := s.project(ctx, ownerID, projectID)
	if err != nil {
		return Issue{}, err
	}
	issue, err := s.GitHub.GetIssue(ctx, repo, number)
	switch {
	case errors.Is(err, github.ErrNotFound):
		return Issue{}, &Refusal{"not_found", "no such issue"}
	case err != nil:
		return Issue{}, &Refusal{"github_unavailable", "GitHub could not be read: " + statusText(err)}
	}
	return Issue{
		Number: issue.Number, Title: issue.Title, Body: issue.Body, State: issue.State,
		Labels: issue.Labels, HTMLURL: issue.HTMLURL,
	}, nil
}

// DependabotAlert is one open Dependabot alert as the agent sees it.
type DependabotAlert struct {
	Number                 int
	PackageName            string
	Ecosystem              string
	Severity               string
	VulnerableVersionRange string
	PatchedVersion         string
	AdvisoryGHSAID         string
	AdvisorySummary        string
	URL                    string
	HTMLURL                string
}

// ListDependabotAlerts lists the open Dependabot alerts of the project's repository. A repository
// with none returns an empty slice, not an error.
func (s *Service) ListDependabotAlerts(ctx context.Context, ownerID, projectID string) ([]DependabotAlert, error) {
	repo, err := s.project(ctx, ownerID, projectID)
	if err != nil {
		return nil, err
	}
	alerts, err := s.GitHub.ListDependabotAlerts(ctx, repo)
	if err != nil {
		return nil, &Refusal{"github_unavailable", "GitHub could not be read: " + statusText(err)}
	}
	out := make([]DependabotAlert, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, DependabotAlert{
			Number: a.Number, PackageName: a.PackageName, Ecosystem: a.Ecosystem, Severity: a.Severity,
			VulnerableVersionRange: a.VulnerableVersionRange, PatchedVersion: a.PatchedVersion,
			AdvisoryGHSAID: a.AdvisoryGHSAID, AdvisorySummary: a.AdvisorySummary, URL: a.URL, HTMLURL: a.HTMLURL,
		})
	}
	return out, nil
}
