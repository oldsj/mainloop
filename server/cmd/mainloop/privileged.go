package main

import (
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/approval"
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
		SSHOn:        bool(cfg.PrivilegedExecutorEnabled) && bool(cfg.PrivilegedSSHEnabled),
	}
}

// newApprovals checks Approve on a privileged action's card: a phone's must carry a token its
// Touch ID approval key signed, and with approval keys pinned for the executor, an executor type is
// approved only from a phone whose key is pinned (docs/specs/privileged-actions.md).
func newApprovals(cfg *config.Config) (*approval.Checker, error) {
	pinned, err := approval.ParsePinned(cfg.PrivilegedApprovalKeys)
	if err != nil {
		return nil, err
	}
	executor := map[string]bool{}
	for _, t := range privileged.TypesFor(true) {
		executor[t] = true
	}
	return &approval.Checker{
		OwnerID: cfg.Identity.OwnerID, Audience: strings.ToLower(cfg.FrontendOrigin()),
		Pinned: pinned, ExecutorTypes: executor,
	}, nil
}
