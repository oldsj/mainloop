// Command mainloop is the Mainloop server binary. Each subcommand is one process role.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/environments"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/gittransport"
	"github.com/oldsj/mainloop/server/internal/health"
	"github.com/oldsj/mainloop/server/internal/hitl"
	"github.com/oldsj/mainloop/server/internal/httpx"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/preview"
	"github.com/oldsj/mainloop/server/internal/progress"
	"github.com/oldsj/mainloop/server/internal/projects"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/pushgate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/schedules"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/tasks"
	"github.com/oldsj/mainloop/server/internal/workspaces"
)

const (
	exitFailure = 1

	mcpAddr     = ":8002"
	gitReadAddr = ":8003"
	gitPushAddr = ":8004"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitFailure
	}
	switch cmd := args[0]; cmd {
	case "serve-api", "serve-mcp", "serve-git":
		log := slog.New(slog.NewJSONHandler(stdout, nil))
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		if err := serve(ctx, cmd, log); err != nil {
			log.Error("exiting", "command", cmd, "error", err.Error())
			return exitFailure
		}
		return 0
	case "migrate":
		return runMigrate(args[1:], stdout, stderr)
	case "kagent-probe":
		return runKagentProbe(context.Background(), stdout, stderr)
	case "sweep-orphans":
		return workspaces.RunSweepOrphans(context.Background(), args[1:], stdout, stderr)
	case "smoke":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return runSmoke(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		usage(stderr)
		return exitFailure
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `usage: mainloop <command>

commands:
  serve-api       owner API on :8000
  serve-mcp       MCP listener on :8002
  serve-git       git transport: upload-pack on :8003, validated receive-pack on :8004
  migrate         up | status | reset the database schema
  sweep-orphans   list kagent Sessions Mainloop created and no longer references (--delete removes them;
                  --forget-agent ns/name drops an Agent from the swept set)
  smoke           --read-only: probe a running Mainloop (no quota); --live --budget N: one task from chat to merge
  kagent-probe    read-only check that kagent answers (GetAgent and ListSessions)
`)
}

func serve(ctx context.Context, cmd string, log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	switch cmd {
	case "serve-api":
		return serveAPI(ctx, cfg, log)
	case "serve-mcp":
		return serveMCP(ctx, cfg, log)
	case "serve-git":
		return serveGitWithPush(ctx, cfg, log)
	}
	return errors.New("unreachable")
}

func serveAPI(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	pool, err := db.NewPool(ctx, cfg, db.WithMaxConns(db.APIMaxConns))
	if err != nil {
		return err
	}
	defer pool.Close()

	// Every service is built here, once, with its collaborators as constructor arguments; nothing is
	// assigned to a service afterwards. Two links would otherwise be circular, and each is broken at
	// its narrow end: the workspace service reads the delivery ledger (sessions.Ledger) instead of the
	// session service, and the health recovery sweep receives the session service's agent resolver
	// when it is registered instead of when the health service is built.
	hub := events.NewHub(log, cfg.Events.SubscriberBuffer)
	gh, err := newGitHubApp(cfg)
	if err != nil {
		return err
	}
	kc, err := kagent.New(kagent.FromSettings(cfg.Kagent))
	if err != nil {
		return err
	}
	reg, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		return err
	}
	creds, err := newCredentials(cfg, pool, log)
	if err != nil {
		return err
	}
	attn := attention.NewService(cfg, pool, log)

	// The one push gate of this process. A renamed default branch advances the push policy whether or
	// not the gate is on: the policy is database state that outlives a gate-off period, and advancing
	// it needs no upstream. With the gate off the upstream is nil and only that policy path works.
	gateOn := pushGateEnabled(cfg)
	var upstream pushgate.Upstream
	if gateOn {
		upstream, err = pushgate.NewGitHubUpstream(github.Options{AppID: cfg.GitHub.AppID, PrivateKey: cfg.GitHub.AppPrivateKey.Reveal()})
		if err != nil {
			return err
		}
	}
	gate := pushgate.NewService(pool, kc, upstream, creds)

	var remote projects.Remote // nil when no GitHub App is configured
	if gh != nil {
		remote = gh
	}
	projectSvc := projects.NewService(cfg, pool, remote, gate, attn.NeedsOwner, log)
	wsOpts := workspaceOptions(cfg, gate, gitReadFor(cfg, pool, creds, log))
	if gh != nil {
		// A workspace kagent prepares starts from one exact commit, so its base ref is resolved here.
		wsOpts = append(wsOpts, workspaces.WithRefs(gh))
	}
	ledger := sessions.NewLedger(cfg, pool)
	wsSvc := workspaces.New(cfg, pool, kc, reg, creds, ledger, projectSvc, log, wsOpts...)
	healthSvc := health.NewService(cfg, pool, kc, log)
	live := progress.New(hub)
	svc := sessions.NewService(cfg, pool, kc, reg, creds, healthSvc, wsSvc, live, log)
	hitlSvc := hitl.NewService(cfg, pool, hitl.FromClient(kc), svc, svc, svc, log)
	schedSvc := schedules.NewService(cfg, pool, kc, reg, log)
	previews, err := preview.New(cfg, pool, wsSvc, log)
	if err != nil {
		return err
	}
	taskOpts := []tasks.Option{
		tasks.WithRuntimes(wsSvc, svc),
		tasks.WithStopObserver(&tasks.KagentStops{Pool: pool, OwnerID: cfg.Identity.OwnerID, Sessions: svc, Kagent: kc, Liveness: kc, Workspaces: wsSvc, Deleter: kc}),
		tasks.WithLivenessObserver(&tasks.KagentLiveness{Pool: pool, OwnerID: cfg.Identity.OwnerID, Sessions: svc, Kagent: kc, Activity: ledger}),
	}
	if gh != nil {
		taskOpts = append(taskOpts, tasks.WithObserver(tasks.GitHubObserver(&github.TaskObserver{App: gh})))
	}
	taskSvc := tasks.NewService(cfg, pool, reg, log, taskOpts...)
	prSvc, mergeSvc := newPullRequestServices(cfg, pool, gh, kc, log)

	handler, err := app.New(app.Deps{
		Config: cfg, Log: log, DB: pool, Pool: pool, Hub: hub, Live: live,
		Providers: reg, Projects: projectSvc, Tasks: taskSvc, Workspaces: wsSvc, Sessions: svc, Preview: previews,
		HITL: hitlSvc, Attention: attn, Health: healthSvc, Schedules: schedSvc, Merge: mergeSvc,
		Devloop: &devloop.LeaseService{Pool: pool},
	}).Handler()
	if err != nil {
		return err
	}

	g, gctx := errgroup.WithContext(ctx)
	// Long-lived SSE streams never finish on their own, so the HTTP server's graceful shutdown
	// would wait out its full timeout for them. Ending them first lets it drain at once.
	stopStreams := context.AfterFunc(gctx, hub.Close)
	defer stopStreams()

	notifier := reconcile.NewNotifier(pool, log)
	source := &events.Source{
		Pool: pool, Hub: hub, Log: log,
		Wake: notifier.Subscribe(events.Channel), Poll: cfg.Events.PollInterval.Duration(),
	}
	runner := reconcile.NewRunner(pool, log, notifier)
	if gateOn {
		gate.Register(runner)
	}
	runner.AddSweep(projectSvc.MetadataSweep())
	runner.AddSweep(events.PruneSweep(pool, time.Duration(cfg.Events.RetentionHours)*time.Hour))
	wsSvc.Register(runner)
	registerCredentials(cfg, creds, runner)
	if cfg.InCluster() {
		(&gittransport.Enroller{Pool: pool, Creds: creds, Log: log}).Register(runner)
	}
	runner.Add(svc.DeliveryLoop())
	runner.AddSweep(svc.EnsureSweep())
	runner.AddSweep(svc.ResolveCreatesSweep())
	runner.AddSweep(svc.CancelSweep())
	runner.AddSweep(svc.PromoteStrandedQueuedSweep())
	runner.AddSweep(svc.RestartSweep())
	runner.AddSweep(svc.StallSweep())
	runner.AddSweep(svc.CheckpointSweep())
	runner.AddSweep(svc.ForkSweep())
	runner.Add(hitlSvc.ResponseLoop())
	runner.AddSweep(hitlSvc.ObserverSweep())
	runner.AddSweep(attn.Sweep())
	runner.AddSweep(healthSvc.RecoverSweep(svc))
	runner.AddSweep(healthSvc.RetainSweep())
	runner.AddSweep(schedSvc.Sweep())
	runner.AddSweep(environments.NewAPI(cfg, pool, nil).Service.FollowSweep(cfg.Identity.OwnerID))
	previews.Register(runner)
	taskSvc.Register(runner)
	devOps, err := newDevloopOperations(cfg, pool)
	if err != nil {
		return err
	}
	if devOps != nil {
		devOps.Register(runner)
	}
	if prSvc != nil {
		runner.Add(prSvc.Loop())
		// Merges run only where the merge tools are on: nothing else creates a candidate.
		if cfg.MergeToolsEnabled {
			runner.Add(mergeSvc.Loop())
			runner.AddSweep(mergeSvc.CompletionSweep())
		}
	}

	g.Go(func() error { return notifier.Run(gctx) })
	g.Go(func() error { return source.Run(gctx) })
	g.Go(func() error { return runner.Run(gctx) })
	g.Go(func() error { return httpx.Serve(gctx, log, "api", cfg.ListenAddr(), handler) })
	return g.Wait()
}

// serveGitWithPush reuses the read listener and adds the gate on the independent push port.
// Disabled push listeners still generate safe correlation IDs and closed refusal diagnostics.
func serveGitWithPush(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
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
		srv := &pushgate.Server{Log: log, AllowedHosts: cfg.GitPushHosts(), Dev: cfg.IsDev(),
			Enabled: pushGateEnabled(cfg),
			Limits:  pushgate.Limits{BodyBytes: cfg.Git.PushGateMaxBodyBytes, CommandBytes: cfg.Git.PushGateMaxCommandBytes}}
		if srv.Enabled {
			pool, err := db.NewPool(gctx, cfg)
			if err != nil {
				return err
			}
			defer pool.Close()
			kc, err := kagent.New(kagent.FromSettings(cfg.Kagent))
			if err != nil {
				return err
			}
			upstream, err := pushgate.NewGitHubUpstream(github.Options{AppID: cfg.GitHub.AppID, PrivateKey: cfg.GitHub.AppPrivateKey.Reveal()})
			if err != nil {
				return err
			}
			// No credential service: serve-git never enrolls a workspace (the API process owns the
			// credential Secrets and runs Enroll), so the gate here only serves pushes and Enroll on
			// it answers pushgate.ErrEnrollUnavailable.
			srv.Service = pushgate.NewService(pool, kc, upstream, nil)
		}
		return httpx.Serve(gctx, log, "git-push", gitPushAddr, srv.Handler())
	})
	return g.Wait()
}
