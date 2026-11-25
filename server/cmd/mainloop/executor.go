package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/oldsj/mainloop/server/internal/approval"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/onepassword"
	"github.com/oldsj/mainloop/server/internal/privileged"
)

const executorEvery = 3 * time.Second

// serveExecutor runs privileged actions that need a credential serve-api must not hold
// (docs/specs/privileged-actions.md). It has no listener: it takes work from PostgreSQL under its
// own role and writes results back, and serve-api opens the cards and delivers the outcomes.
func serveExecutor(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.LoadExecutor()
	if err != nil {
		return err
	}
	pool, err := db.NewPoolFromURL(ctx, cfg.DB.URL(), db.WithMaxConns(4))
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := checkExecutorRole(ctx, pool); err != nil {
		return err
	}
	op, err := onepassword.New(ctx, cfg.OnePasswordToken.Reveal(), "1")
	if err != nil {
		return err
	}
	kc, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("executor: in-cluster Kubernetes config: %w", err)
	}
	kube, err := kubernetes.NewForConfig(kc)
	if err != nil {
		return fmt.Errorf("executor: Kubernetes client: %w", err)
	}
	svc := &privileged.Service{
		Pool: pool, OwnerID: cfg.OwnerID, Log: log, Executor: true,
		OnePassword: op, Secrets: privileged.KubeSecrets{Client: kube}, SecretNamespaces: cfg.SecretNamespaces,
		ExecutorNamespace: cfg.Namespace, Cluster: cfg.Cluster,
	}
	if cfg.Kubernetes {
		clients := privileged.KubeClients{Client: kube, Base: kc, Namespace: cfg.Namespace}
		svc.Kube = privileged.NewKubeActions(clients)
		svc.Kubectl = privileged.ExecKubectl{Clients: clients, Dir: os.TempDir()}
	}
	if cfg.Omni {
		key, err := privileged.DecodeOmniKey(cfg.OmniKey.Reveal())
		if err != nil {
			return fmt.Errorf("executor: MAINLOOP_EXECUTOR_OMNI_KEY: %w", err)
		}
		svc.Sidero = privileged.ExecSidero{Endpoint: cfg.OmniEndpoint, Key: key, Clusters: cfg.OmniClusters, Dir: os.TempDir()}
		svc.OmniClusters = cfg.OmniClusters
	}
	if svc.ApprovalKeys, err = approval.ParsePinned(cfg.ApprovalKeys); err != nil {
		return err
	}
	svc.ApprovalsRequired = bool(cfg.ApprovalKeysRequired)
	if err := checkApprovalKeys(log, true, svc.ApprovalsRequired, svc.ApprovalKeys); err != nil {
		return err
	}
	if cfg.SSH {
		hosts := map[string]netip.AddrPort{}
		for _, h := range cfg.SSHHosts {
			hosts[h.Name] = h.Address
			svc.SSHHosts = append(svc.SSHHosts, h.Name)
		}
		svc.SSH = privileged.ExecSSH{Hosts: hosts, Dir: cfg.SSHDir}
	}
	if cfg.UniFi {
		svc.UniFi = privileged.ExecUniFi{Address: cfg.UniFiAddress, Dir: cfg.UniFiDir}
	}
	r := &privileged.Runner{Service: svc}
	log.Info("executor ready", "types", privileged.TypesFor(true), "secret_namespaces", cfg.SecretNamespaces,
		"kubernetes", bool(cfg.Kubernetes), "namespace", cfg.Namespace, "omni", bool(cfg.Omni), "omni_clusters", cfg.OmniClusters,
		"ssh_hosts", svc.SSHHosts, "unifi", bool(cfg.UniFi),
		"pinned_approval_keys", len(svc.ApprovalKeys), "approval_keys_required", svc.ApprovalsRequired)
	t := time.NewTicker(executorEvery)
	defer t.Stop()
	for {
		if err := r.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error("executor pass failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// checkExecutorRole refuses to run under a database role that can do more than the executor needs:
// one that reads any table but its own two is serve-api's, or a superuser.
func checkExecutorRole(ctx context.Context, pool *pgxpool.Pool) error {
	var own, other bool
	err := pool.QueryRow(ctx, `SELECT has_table_privilege('privileged_actions', 'SELECT'),
		has_table_privilege('attention_items', 'SELECT')`).Scan(&own, &other)
	switch {
	case err != nil:
		return fmt.Errorf("executor: check database role: %w", err)
	case other:
		return errors.New("executor: its database role can read attention_items; run it as the mainloop_executor role only")
	case !own:
		return errors.New("executor: its database role has no grants yet; `mainloop migrate up` grants them once the mainloop_executor role exists")
	}
	return nil
}
