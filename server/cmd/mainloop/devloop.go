package main

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/pushgate"
)

// newDevloopOperations builds the full [devloop.OperationsService], with a real Kubernetes
// client and a GitHub seeder. Only serve-api calls this: it is the one process with a Kubernetes
// client, per internal/devloop's package doc. It returns (nil, nil) when the flag is off or this
// process is not in a cluster; devloop is optional, so that is not a startup failure.
func newDevloopOperations(cfg *config.Config, pool *pgxpool.Pool) (*devloop.OperationsService, error) {
	if !bool(cfg.DevloopEnabled) || !cfg.InCluster() {
		return nil, nil
	}
	pods, err := devloop.NewInClusterPods()
	if err != nil {
		return nil, fmt.Errorf("devloop: %w", err)
	}
	svc := &devloop.OperationsService{Pool: pool, Pods: pods}
	if cfg.GitHub.AppID != "" {
		seed, err := pushgate.NewGitHubUpstream(github.Options{AppID: cfg.GitHub.AppID, PrivateKey: cfg.GitHub.AppPrivateKey.Reveal()})
		if err != nil {
			return nil, fmt.Errorf("devloop: %w", err)
		}
		svc.Seed = seed
	}
	return svc, nil
}
