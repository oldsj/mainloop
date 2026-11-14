package main

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/ci"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/issues"
)

// newIssuesService builds the issue and Dependabot-alert read service over the same
// GitHub App the pull-request tools use. With no App it is nil, and the tools answer that they
// are unavailable.
func newIssuesService(pool *pgxpool.Pool, gh *github.App) *issues.Service {
	if gh == nil {
		return nil
	}
	return issues.NewService(pool, gh)
}

// newCIService builds the CI read service over the same GitHub App. With no App it is nil, and
// the check tools answer that they are unavailable.
func newCIService(pool *pgxpool.Pool, gh *github.App) *ci.Service {
	if gh == nil {
		return nil
	}
	return ci.NewService(pool, gh)
}
