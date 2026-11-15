package main

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/dotfiles"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/pushgate"
)

// newDotfilesExports reads agent dotfiles sources through the GitHub App: refs through gh, commits
// through the App's git upstream.
func newDotfilesExports(cfg *config.Config, pool *pgxpool.Pool, gh *github.App, log *slog.Logger) (*dotfiles.Exports, error) {
	seeder, err := pushgate.NewGitHubUpstream(github.Options{AppID: cfg.GitHub.AppID, PrivateKey: cfg.GitHub.AppPrivateKey.Reveal()})
	if err != nil {
		return nil, err
	}
	return &dotfiles.Exports{Pool: pool, Refs: gh, Seeder: seeder, Log: log}, nil
}
