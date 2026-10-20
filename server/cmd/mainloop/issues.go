package main

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/issues"
)

// newIssuesService builds the issue, Dependabot-alert and check-run read service over the same
// GitHub App the pull-request tools use. With no App it is nil, and the tools answer that they
// are unavailable.
func newIssuesService(pool *pgxpool.Pool, gh *github.App) *issues.Service {
	if gh == nil {
		return nil
	}
	return issues.NewService(pool, gh)
}
