package main

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/merge"
	"github.com/oldsj/mainloop/server/internal/prs"
)

// newPullRequestServices builds the pull-request and merge services over the GitHub App. Both the
// API process (which runs their loops) and the MCP process (whose tools write the intent rows and
// wait for the loops to settle them) build them the same way. With no App they are nil, and the
// tools answer that they are unavailable. rt is the trusted kagent read that gives a merge tool its
// authority.
func newPullRequestServices(cfg *config.Config, pool *pgxpool.Pool, gh *github.App, rt merge.Runtime, log *slog.Logger) (*prs.Service, *merge.Service) {
	if gh == nil {
		return nil, nil
	}
	return &prs.Service{Pool: pool, GitHub: gh, Log: log},
		&merge.Service{Pool: pool, GitHub: gh, Log: log, Configs: cfg.MergeConfigurations, Runtime: rt}
}

// newGitHubApp builds the GitHub App client, or nil when no App is configured.
func newGitHubApp(cfg *config.Config) (*github.App, error) {
	if cfg.GitHub.AppID == "" {
		return nil, nil
	}
	return github.NewApp(github.Options{AppID: cfg.GitHub.AppID, PrivateKey: cfg.GitHub.AppPrivateKey.Reveal()})
}
