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

	"golang.org/x/sync/errgroup"

	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/httpx"
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
	case "sweep-orphans", "smoke", "kagent-probe":
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
  serve-mcp       MCP listener on :8002 (health only for now)
  serve-git       git transport on :8003 and :8004 (health only for now)
  migrate         up | status | reset the database schema
  sweep-orphans   not implemented yet
  smoke           not implemented yet
  kagent-probe    not implemented yet
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
		return httpx.Serve(ctx, log, "mcp", mcpAddr, app.StubHandler(cfg, log, cfg.AllowedMCPHosts()))
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

	deps := app.Deps{Config: cfg, Log: log, DB: pool}
	handler, err := app.New(deps).Handler()
	if err != nil {
		return err
	}
	return httpx.Serve(ctx, log, "api", cfg.ListenAddr(), handler)
}
