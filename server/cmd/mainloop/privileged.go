package main

import (
	"errors"
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
		UniFiOn:      bool(cfg.PrivilegedExecutorEnabled) && bool(cfg.PrivilegedUniFiEnabled),
	}
}

// newApprovals checks Approve on a privileged action's card: a phone's must carry a token its
// Touch ID approval key signed, and with approval keys pinned for the executor, an executor type is
// approved only from a phone whose key is pinned (docs/specs/privileged-actions.md).
func newApprovals(cfg *config.Config, log *slog.Logger) (*approval.Checker, error) {
	pinned, err := approval.ParsePinned(cfg.PrivilegedApprovalKeys)
	if err != nil {
		return nil, err
	}
	mode := cfg.PrivilegedApprovalsMode
	if err := checkApprovalKeys(log, bool(cfg.PrivilegedExecutorEnabled), mode, pinned); err != nil {
		return nil, err
	}
	executor := map[string]bool{}
	for _, t := range privileged.TypesFor(true) {
		executor[t] = true
	}
	return &approval.Checker{
		OwnerID: cfg.Identity.OwnerID, Audience: strings.ToLower(cfg.FrontendOrigin()),
		Pinned: pinned, Required: mode == config.ApprovalsSigned, ExecutorTypes: executor,
	}, nil
}

// checkApprovalKeys fails closed on the approval settings both processes start with. An executor
// needs its mode chosen (web or signed), so dropping the keys can't quietly fall back to web
// approvals; signed needs a pinned key; web must pin none, as a pinned key would make the executor
// refuse the web app's approvals the mode promises. In web mode with an executor it warns at every
// start: the executor runs approvals no phone signed. serve-api and serve-executor both call it
// with the same values.
func checkApprovalKeys(log *slog.Logger, executor bool, mode config.ApprovalsMode, pinned approval.Pinned) error {
	switch {
	case mode == config.ApprovalsUnset && executor:
		return errors.New("MAINLOOP_PRIVILEGED_APPROVALS_MODE must be set to web or signed when an executor is deployed")
	case mode == config.ApprovalsSigned && len(pinned) == 0:
		return errors.New("signed approvals are required (MAINLOOP_PRIVILEGED_APPROVALS_MODE=signed) and MAINLOOP_PRIVILEGED_APPROVAL_KEYS pins no key")
	case mode == config.ApprovalsWeb && len(pinned) > 0:
		return errors.New("MAINLOOP_PRIVILEGED_APPROVALS_MODE=web pins approval keys in MAINLOOP_PRIVILEGED_APPROVAL_KEYS; set the mode to signed, or pin none")
	case mode == config.ApprovalsWeb && executor:
		log.Warn("UNSIGNED PRIVILEGED APPROVALS: the approval mode is web, so the executor runs any approval serve-api records, including the web app's, with no Touch ID signature to check; pin a phone's key in executor.approvalKeys.pinned and set executor.approvals.mode to signed")
	}
	return nil
}
