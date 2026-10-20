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
	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/httpx"
	"github.com/oldsj/mainloop/server/internal/k8ssecrets"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
	"github.com/oldsj/mainloop/server/internal/providers"
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

	reg, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		return err
	}
	taskSvc := tasks.NewService(cfg, pool, reg, log)
	gh, err := newGitHubApp(cfg)
	if err != nil {
		return err
	}
	kc, err := kagent.New(kagent.FromSettings(cfg.Kagent))
	if err != nil {
		return err
	}
	prSvc, mergeSvc := newPullRequestServices(cfg, pool, gh, kc, log)
	issuesSvc := newIssuesService(pool, gh)
	q := dbq.New(pool)
	// The lease is DB-only and the operations service here carries no Kubernetes client or GitHub
	// seeder: this process only inserts and polls devloop_operations rows (see
	// internal/devloop's package doc); serve-api's instance, built in newDevloopOperations, is the
	// one that actually reaches Kubernetes and GitHub.
	srv := &mcpserver.Server{
		PRs:          prSvc,
		Merge:        mergeSvc,
		Issues:       issuesSvc,
		DevLease:     &devloop.LeaseService{Pool: pool},
		DevOps:       &devloop.OperationsService{Pool: pool},
		DevTargets:   &devloop.TargetService{Pool: pool},
		Auth:         &agentauth.Authenticator{Q: q, Depth: taskSvc.AuthDepth},
		Tasks:        taskSvc,
		Policy:       agentauth.Policy{MergeTools: bool(cfg.MergeToolsEnabled), DevLoop: bool(cfg.DevloopEnabled)},
		Queries:      q,
		Topics:       topics.NewService(pool),
		Log:          log,
		AllowedHosts: cfg.AllowedMCPHosts(),
		Dev:          cfg.IsDev(),
	}
	handler, err := srv.Handler()
	if err != nil {
		return err
	}
	return httpx.Serve(ctx, log, "mcp", mcpAddr, handler)
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
	return agentauth.NewCredentials(pool, store, []byte(cfg.Identity.AgentTokenKey.Reveal()), cfg.CredentialSecretPrefix(), log), nil
}

// registerCredentials adds the cleanup loop to the runner when the credentials can reach
// Kubernetes.
func registerCredentials(cfg *config.Config, creds *agentauth.Credentials, runner *reconcile.Runner) {
	if cfg.InCluster() {
		creds.Register(runner)
	}
}
