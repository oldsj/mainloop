package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/httpx"
	"github.com/oldsj/mainloop/server/internal/k8ssecrets"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/tasks"
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

	taskSvc, err := tasks.NewService(cfg, pool)
	if err != nil {
		return err
	}
	gh, err := newGitHubApp(cfg)
	if err != nil {
		return err
	}
	kc, err := kagent.New(kagent.FromSettings(cfg.Kagent))
	if err != nil {
		return err
	}
	prSvc, mergeSvc := newPullRequestServices(cfg, pool, gh, kc, log)
	q := dbq.New(pool)
	srv := &mcpserver.Server{
		PRs:          prSvc,
		Merge:        mergeSvc,
		Auth:         &agentauth.Authenticator{Q: q, Depth: taskSvc.AuthDepth},
		Tasks:        taskSvc,
		Policy:       agentauth.Policy{MergeTools: bool(cfg.MergeToolsEnabled)},
		Queries:      q,
		Topics:       &topics.Service{Pool: pool},
		Log:          log,
		AllowedHosts: cfg.AllowedMCPHosts(),
		Dev:          cfg.IsDev(),
	}
	return httpx.Serve(ctx, log, "mcp", mcpAddr, srv.Handler())
}

// newCredentials builds the one credential service of this process. In a cluster it publishes
// Secrets to the kagent namespace, and failing to reach Kubernetes stops startup: running on
// without it would silently leave revoked Secrets behind. Outside a cluster (local development)
// its Issue fails with [agentauth.ErrNotConfigured] and nothing cleans up, since there are no
// Secrets to clean.
func newCredentials(cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger) (*agentauth.Credentials, error) {
	return newCredentialsWith(cfg, pool, log, func(ns string) (agentauth.SecretStore, error) {
		return k8ssecrets.NewInCluster(ns)
	})
}

func newCredentialsWith(cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger, inCluster func(namespace string) (agentauth.SecretStore, error)) (*agentauth.Credentials, error) {
	var store agentauth.SecretStore = agentauth.Unconfigured{}
	if cfg.InCluster() {
		s, err := inCluster(cfg.Kagent.Namespace)
		if err != nil {
			return nil, fmt.Errorf("credentials: %w", err)
		}
		store = s
	} else {
		log.Warn("credential cleanup is off and credentials cannot be issued: not running in a cluster")
	}
	return &agentauth.Credentials{
		Pool: pool, Secrets: store, Key: []byte(cfg.Identity.AgentTokenKey.Reveal()),
		Prefix: cfg.CredentialSecretPrefix(), Log: log,
	}, nil
}

// registerCredentials adds the cleanup loop to the runner when the credentials can reach
// Kubernetes.
func registerCredentials(cfg *config.Config, creds *agentauth.Credentials, runner *reconcile.Runner) {
	if cfg.InCluster() {
		creds.Register(runner)
	}
}
