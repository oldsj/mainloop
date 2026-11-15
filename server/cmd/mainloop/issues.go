package main

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/ci"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/issues"
)

// newIssuesService builds the issue and Dependabot-alert service over the same GitHub App the
// pull-request tools use. The MCP process's tools read and write intent rows with it; the API
// process runs its write loop. With no App it is nil, and the tools answer that they are
// unavailable.
func newIssuesService(pool *pgxpool.Pool, gh *github.App, log *slog.Logger) *issues.Service {
	if gh == nil {
		return nil
	}
	return issues.NewService(pool, gh, log)
}

// newCIService builds the CI read service over the same GitHub App. With no App it is nil, and
// the check tools answer that they are unavailable.
func newCIService(pool *pgxpool.Pool, gh *github.App) *ci.Service {
	if gh == nil {
		return nil
	}
	return ci.NewService(pool, gh)
}
