package main

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/dotfiles"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/ownermemory"
	"github.com/oldsj/mainloop/server/internal/pushgate"
)

// newDotfilesExports reads agent dotfiles sources through the GitHub App: refs through gh, commits
// through the App's git upstream. Each ready export of an owner's own source is checked against
// their owner memory, flagging the entries its AGENTS.md already holds.
func newDotfilesExports(cfg *config.Config, pool *pgxpool.Pool, gh *github.App, log *slog.Logger) (*dotfiles.Exports, error) {
	seeder, err := pushgate.NewGitHubUpstream(github.Options{AppID: cfg.GitHub.AppID, PrivateKey: cfg.GitHub.AppPrivateKey.Reveal()})
	if err != nil {
		return nil, err
	}
	return &dotfiles.Exports{
		Pool: pool, Refs: gh, Seeder: seeder, Log: log,
		OwnerExported: func(ctx context.Context, owner, commit string, agentsMD []byte) error {
			return ownermemory.CheckDotfiles(ctx, pool, owner, commit, agentsMD)
		},
	}, nil
}
