package main

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/httpx"
	"github.com/oldsj/mainloop/server/internal/k8ssecrets"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/topics"
)

// serveMCP runs the MCP listener. It reads the database and never touches Kubernetes: the API
// process owns the credential Secrets.
func serveMCP(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	pool, err := db.NewPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	q := dbq.New(pool)
	srv := &mcpserver.Server{
		Auth:         &agentauth.Authenticator{Q: q},
		Policy:       agentauth.Policy{MergeTools: bool(cfg.MergeToolsEnabled)},
		Queries:      q,
		Topics:       &topics.Service{Pool: pool},
		Log:          log,
		AllowedHosts: cfg.AllowedMCPHosts(),
		Dev:          cfg.IsDev(),
	}
	return httpx.Serve(ctx, log, "mcp", mcpAddr, srv.Handler())
}

// registerCredentials adds the credential cleanup loop to the runner. Secrets exist only in
// the cluster, so outside one (local development) the loop is off and revoked credentials wait
// for a process that can reach Kubernetes.
func registerCredentials(cfg *config.Config, pool *pgxpool.Pool, runner *reconcile.Runner, log *slog.Logger) {
	if !cfg.InCluster() {
		log.Warn("credential cleanup is off: not running in a cluster")
		return
	}
	store, err := k8ssecrets.NewInCluster(cfg.Kagent.Namespace)
	if err != nil {
		log.Error("credential cleanup is off", "error", err.Error())
		return
	}
	creds := &agentauth.Credentials{
		Pool: pool, Secrets: store, Key: []byte(cfg.Identity.AgentTokenKey.Reveal()),
		Prefix: cfg.CredentialSecretPrefix(), Log: log,
	}
	creds.Register(runner)
}
