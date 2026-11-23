package main

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/ci"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/environments"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/onboarding"
	"github.com/oldsj/mainloop/server/internal/projects"
)

// newOnboarding builds the project_propose service over the same GitHub App (nil when this
// Mainloop has none: proposals then say the installation was not checked) and the anonymous
// registry reader environment registration uses.
func newOnboarding(cfg *config.Config, pool *pgxpool.Pool, gh *github.App, log *slog.Logger) *onboarding.Service {
	svc := &onboarding.Service{
		Pool: pool, OwnerID: cfg.Identity.OwnerID, Log: log,
		Images: &environments.Validator{
			Client: &environments.AnonymousRegistry{}, Allowlist: cfg.EnvironmentRegistryAllowlist,
			Budgets: environments.DefaultBudgets(),
		},
	}
	if gh != nil {
		svc.GitHub = gh
	}
	return svc
}

// newProposalApplier builds the loop that applies approved project proposals through the same
// project, environment and checks services the owner's routes use. The checks mode is a database
// write, so it needs no GitHub App; without one the App's installation is not re-checked.
func newProposalApplier(cfg *config.Config, pool *pgxpool.Pool, projectSvc *projects.Service, ciSvc *ci.Service, gh *github.App, log *slog.Logger) *onboarding.Applier {
	if ciSvc == nil {
		ciSvc = ci.NewService(pool, nil)
	}
	a := &onboarding.Applier{
		Pool: pool, OwnerID: cfg.Identity.OwnerID, Projects: projectSvc, Verification: ciSvc,
		Environments: environments.NewAPI(cfg, pool, nil).Service, RegistryAllowlist: cfg.EnvironmentRegistryAllowlist,
		Log: log,
	}
	if gh != nil {
		a.GitHub = gh
	}
	return a
}
