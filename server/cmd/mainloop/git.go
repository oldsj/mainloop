package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/gittransport"
	"github.com/oldsj/mainloop/server/internal/workspaces"
)

// pushGateEnabled reports whether the push gate takes pushes: it needs the transport, the gate switch
// and a GitHub App for the upstream.
func pushGateEnabled(cfg *config.Config) bool {
	return cfg.Git.TransportEnabled && cfg.Git.PushGateEnabled && cfg.GitHub.AppID != ""
}

func gitReadHandler(ctx context.Context, cfg *config.Config, log *slog.Logger) (handler http.Handler, closeFn func(), err error) {
	if !cfg.Git.TransportEnabled || cfg.GitHub.AppID == "" {
		log.Warn("git read transport is off: it needs GIT_TRANSPORT_ENABLED and a GitHub App")
		return app.StubHandler(cfg, log, cfg.GitReadHosts()), func() {}, nil
	}
	pool, err := db.NewPool(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	gh, err := github.NewApp(github.Options{AppID: cfg.GitHub.AppID, PrivateKey: cfg.GitHub.AppPrivateKey.Reveal()})
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("git read transport: %w", err)
	}
	srv := &gittransport.Server{
		Auth:         &gittransport.Authenticator{Q: dbq.New(pool)},
		Upstream:     gh,
		Log:          log,
		Ready:        pool,
		AllowedHosts: cfg.GitReadHosts(),
		Dev:          cfg.IsDev(),
	}
	return srv.Handler(), pool.Close, nil
}

// gitReadFor returns the git read capability workspaces are created with, or nil when it is off.
// It needs the transport enabled, a GitHub App for the read Service to fetch with, and a cluster
// to publish the capability's Secret in.
func gitReadFor(cfg *config.Config, pool *pgxpool.Pool, creds *agentauth.Credentials, log *slog.Logger) *workspaces.GitRead {
	if !cfg.Git.TransportEnabled || cfg.GitHub.AppID == "" || !cfg.InCluster() {
		return nil
	}
	return &workspaces.GitRead{
		Enroller: &gittransport.Enroller{Pool: pool, Creds: creds, Log: log},
		Origin:   cfg.Git.ReadOrigin,
	}
}
