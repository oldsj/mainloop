package main

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/privileged"
)

// newPrivileged builds serve-api's privileged-action service over Mainloop's GitHub App, or nil
// when privileged actions are off (or owner authentication is not enforced) or there is no App: the
// tools then answer that privileged actions are unavailable. Executor types are accepted only when
// a serve-executor is deployed (MAINLOOP_PRIVILEGED_EXECUTOR_ENABLED).
func newPrivileged(cfg *config.Config, pool *pgxpool.Pool, gh *github.App, log *slog.Logger) *privileged.Service {
	if !cfg.PrivilegedActionsOn() || gh == nil {
		return nil
	}
	return &privileged.Service{
		Pool: pool, OwnerID: cfg.Identity.OwnerID, GitHub: gh, Log: log, ExecutorOn: bool(cfg.PrivilegedExecutorEnabled),
		KubernetesOn: bool(cfg.PrivilegedExecutorEnabled) && bool(cfg.PrivilegedKubernetesEnabled),
		OmniOn:       bool(cfg.PrivilegedExecutorEnabled) && bool(cfg.PrivilegedOmniEnabled),
	}
}
