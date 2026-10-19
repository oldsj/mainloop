// Package issues serves the read-only GitHub issue and Dependabot-alert tools over the project's
// bound repository. Unlike prs and merge, it writes nothing: a read that fails is simply retried
// by the caller, so there is no durable intent row or reconcile loop here.
package issues

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/github"
)

// GitHub is the part of the GitHub App the issue and alert reads use.
type GitHub interface {
	GetIssue(ctx context.Context, repo github.Repo, number int) (*github.Issue, error)
	ListDependabotAlerts(ctx context.Context, repo github.Repo) ([]github.DependabotAlert, error)
}

// ErrNotFound means the project is not the owner's.
var ErrNotFound = errors.New("issues: project not found")

// Refusal is a request Mainloop refused. Code is stable.
type Refusal struct{ Code, Message string }

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// Service reads issues and Dependabot alerts.
type Service struct {
	Pool   *pgxpool.Pool
	GitHub GitHub
}

// NewService builds the service the MCP tools use.
func NewService(pool *pgxpool.Pool, gh GitHub) *Service {
	return &Service{Pool: pool, GitHub: gh}
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
