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
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/httpx"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/preview"
	"github.com/oldsj/mainloop/server/internal/reconcile"
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
  serve-git       git transport on :8003 and :8004 (health only for now)
  migrate         up | status | reset the database schema
  sweep-orphans   list kagent Sessions Mainloop created and no longer references (--delete removes them)
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
		// Git clients address the read and push Services by name; neither is the owner API.
		hosts := map[string]bool{
			"mainloop-git-read." + cfg.Namespace + ".svc.cluster.local": true,
			"mainloop-git-push." + cfg.Namespace + ".svc.cluster.local": true,
		}
		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return httpx.Serve(gctx, log, "git-read", gitReadAddr, app.StubHandler(cfg, log, hosts))
		})
		g.Go(func() error {
			return httpx.Serve(gctx, log, "git-push", gitPushAddr, app.StubHandler(cfg, log, hosts))
		})
		return g.Wait()
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
	if cfg.GitHub.AppID != "" {
		gh, err := github.NewApp(github.Options{AppID: cfg.GitHub.AppID, PrivateKey: cfg.GitHub.AppPrivateKey.Reveal()})
		if err != nil {
			return err
		}
		deps.GitHub = gh
	}
	wsSvc, err := workspaces.New(cfg, pool, log)
	if err != nil {
		return err
	}
	deps.Workspaces = wsSvc
	creds, err := newCredentials(cfg, pool, log)
	if err != nil {
		return err
	}
	deps.Credentials = creds
	kc, err := kagent.New(kagent.FromSettings(cfg.Kagent))
	if err != nil {
		return err
	}
	svc := sessions.NewService(cfg, pool, kc, log)
	deps.Sessions = svc
	previews, err := preview.New(cfg, pool, wsSvc, log)
	if err != nil {
		return err
	}
	deps.Preview = previews
	// Sends wake a suspended workspace through the workspace service, and idle suspend reads the
	// delivery ledger: no turn is interrupted, and a delivery counts as activity.
	svc.Providers, svc.Waker = wsSvc.Providers, wsSvc
	wsSvc.Deliveries = svc
	handler, err := app.New(deps).Handler()
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
	runner.AddSweep(events.PruneSweep(pool, time.Duration(cfg.Events.RetentionHours)*time.Hour))
	wsSvc.Register(runner)
	registerCredentials(cfg, creds, runner)
	runner.Add(svc.DeliveryLoop())
	runner.AddSweep(svc.EnsureSweep())
	runner.AddSweep(svc.ResolveCreatesSweep())
	previews.Register(runner)

	g.Go(func() error { return notifier.Run(gctx) })
	g.Go(func() error { return source.Run(gctx) })
	g.Go(func() error { return runner.Run(gctx) })
	g.Go(func() error { return httpx.Serve(gctx, log, "api", cfg.ListenAddr(), handler) })
	return g.Wait()
}
