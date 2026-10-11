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
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/gittransport"
	"github.com/oldsj/mainloop/server/internal/health"
	"github.com/oldsj/mainloop/server/internal/hitl"
	"github.com/oldsj/mainloop/server/internal/httpx"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/preview"
	"github.com/oldsj/mainloop/server/internal/pushgate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/schedules"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/workspaces"
)

const (
	exitFailure        = 1
	exitNotImplemented = 2

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
		_, _ = fmt.Fprintf(stderr, "%s: not implemented yet\n", cmd)
		return exitNotImplemented
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
  smoke           not implemented yet
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
	pool, err := db.NewPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	hub := events.NewHub(log, cfg.Events.SubscriberBuffer)
	deps := app.Deps{Config: cfg, Log: log, DB: pool, Pool: pool, Hub: hub}
	gh, err := newGitHubApp(cfg)
	if err != nil {
		return err
	}
	if gh != nil {
		deps.GitHub = gh
		deps.TaskObserver = &github.TaskObserver{App: gh}
	}
	kc, err := kagent.New(kagent.FromSettings(cfg.Kagent))
	if err != nil {
		return err
	}
	prSvc, mergeSvc := newPullRequestServices(cfg, pool, gh, kc, log)
	deps.Merge = mergeSvc
	// The one credential service of this process is built first: the workspace and session services
	// publish an agent's MCP credential before creating its kagent Session and revoke it when the
	// agent ends.
	creds, err := newCredentials(cfg, pool, log)
	if err != nil {
		return err
	}
	deps.Credentials = creds
	wsSvc, err := workspaces.New(cfg, pool, log)
	if err != nil {
		return err
	}
	wsSvc.Credentials = creds
	wsSvc.GitRead = gitReadFor(cfg, pool, creds, log)
	deps.Workspaces = wsSvc
	svc := sessions.NewService(cfg, pool, kc, log)
	svc.Credentials = creds
	deps.Sessions = svc
	hitlSvc := hitl.NewService(cfg, pool, hitl.FromClient(kc), log)
	hitlSvc.Agents = svc // one rule for which Agent runs a session
	hitlSvc.Ready = svc  // wake and resume the Session before an answer is sent
	deps.HITL = hitlSvc
	attn := &attention.Service{Pool: pool, OwnerID: cfg.Identity.OwnerID, Log: log}
	deps.Attention = attn
	healthSvc := health.NewService(cfg, pool, log)
	healthSvc.Kagent = kc
	healthSvc.Agents = svc
	svc.Health = healthSvc // a finished turn stores its health observations in the transaction that ends it
	deps.Health = healthSvc
	schedSvc := &schedules.Service{Pool: pool, Kagent: kc, OwnerID: cfg.Identity.OwnerID, Log: log}
	deps.Schedules = schedSvc
	previews, err := preview.New(cfg, pool, wsSvc, log)
	if err != nil {
		return err
	}
	deps.Preview = previews
	// Sends wake a suspended workspace through the workspace service, and idle suspend reads the
	// delivery ledger: no turn is interrupted, and a delivery counts as activity.
	svc.Providers, svc.Waker = wsSvc.Providers, wsSvc
	schedSvc.Providers = wsSvc.Providers
	wsSvc.Deliveries = svc
	apiServer := app.New(deps)
	handler, err := apiServer.Handler()
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
	if cfg.Git.TransportEnabled && cfg.Git.PushGateEnabled && cfg.GitHub.AppID != "" {
		upstream, err := pushgate.NewGitHubUpstream(github.Options{AppID: cfg.GitHub.AppID, PrivateKey: cfg.GitHub.AppPrivateKey.Reveal()})
		if err != nil {
			return err
		}
		(&pushgate.Service{Pool: pool, Native: kc, Upstream: upstream, Credentials: creds}).Register(runner)
	}
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
	runner.Add(hitlSvc.ResponseLoop())
	runner.AddSweep(hitlSvc.ObserverSweep())
	runner.AddSweep(attn.Sweep())
	runner.AddSweep(healthSvc.RecoverSweep())
	runner.AddSweep(healthSvc.RetainSweep())
	runner.AddSweep(schedSvc.Sweep())
	previews.Register(runner)
	if ts := apiServer.TasksAPI.Svc; ts != nil {
		ts.Register(runner)
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
			Enabled: cfg.Git.TransportEnabled && cfg.Git.PushGateEnabled && cfg.GitHub.AppID != "",
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
			srv.Service = &pushgate.Service{Pool: pool, Native: kc, Upstream: upstream}
		}
		return httpx.Serve(gctx, log, "git-push", gitPushAddr, srv.Handler())
	})
	return g.Wait()
}
