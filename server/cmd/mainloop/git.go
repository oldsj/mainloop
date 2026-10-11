package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/gittransport"
	"github.com/oldsj/mainloop/server/internal/httpx"
	"github.com/oldsj/mainloop/server/internal/workspaces"
)

// serveGit runs the two git listeners. The read listener proxies upload-pack for enrolled
// workspaces when the transport is enabled and the GitHub App is configured; otherwise it answers
// only its probes. The push listener is the push gate's and answers only its probes for now.
func serveGit(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		handler, closeFn, err := gitReadHandler(gctx, cfg, log)
		if err != nil {
			return err
		}
		defer closeFn()
		return httpx.Serve(gctx, log, "git-read", gitReadAddr, handler)
	})
	g.Go(func() error {
		return httpx.Serve(gctx, log, "git-push", gitPushAddr, app.StubHandler(cfg, log, cfg.GitPushHosts()))
	})
	return g.Wait()
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
