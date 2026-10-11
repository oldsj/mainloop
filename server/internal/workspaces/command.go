package workspaces

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/providers"
)

const (
	exitFailure   = 1
	orphanTimeout = 5 * time.Minute
)

// New builds the workspace service over the database and kagent.
func New(cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger) (*Service, error) {
	client, err := kagent.New(kagent.FromSettings(cfg.Kagent))
	if err != nil {
		return nil, err
	}
	reg, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		return nil, err
	}
	return &Service{
		Pool: pool, Kagent: client, Providers: reg, Config: cfg, Log: log, OwnerID: cfg.Identity.OwnerID,
	}, nil
}

// RunSweepOrphans lists the kagent Sessions Mainloop created and no longer references. It deletes
// nothing unless --delete is given.
func RunSweepOrphans(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sweep-orphans", flag.ContinueOnError)
	fs.SetOutput(stderr)
	del := fs.Bool("delete", false, "delete the orphans (default: list only)")
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}
	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "sweep-orphans: %v\n", err)
		return exitFailure
	}
	ctx, cancel := context.WithTimeout(ctx, orphanTimeout)
	defer cancel()
	pool, err := db.NewPool(ctx, cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "sweep-orphans: %v\n", err)
		return exitFailure
	}
	defer pool.Close()
	svc, err := New(cfg, pool, slog.New(slog.NewTextHandler(stderr, nil)))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "sweep-orphans: %v\n", err)
		return exitFailure
	}
	rep, err := svc.SweepOrphans(ctx, OrphanOptions{Delete: *del})
	return printOrphans(rep, err, *del, stdout, stderr)
}

func printOrphans(rep OrphanReport, err error, del bool, stdout, stderr io.Writer) int {
	deleted := map[string]bool{}
	for _, id := range rep.Deleted {
		deleted[id] = true
	}
	for _, id := range rep.Orphans {
		switch {
		case deleted[id]:
			_, _ = fmt.Fprintf(stdout, "deleted %s\n", id)
		case rep.Failed[id] != "":
			_, _ = fmt.Fprintf(stdout, "failed  %s: %s\n", id, rep.Failed[id])
		case del && rep.Blocked == "":
			_, _ = fmt.Fprintf(stdout, "kept    %s (bound since the listing)\n", id)
		default:
			_, _ = fmt.Fprintf(stdout, "orphan  %s\n", id)
		}
	}
	if len(rep.Orphans) == 0 {
		_, _ = fmt.Fprintln(stdout, "no orphans")
	} else if !del {
		_, _ = fmt.Fprintf(stdout, "%d orphan(s); nothing deleted (pass --delete to remove them)\n", len(rep.Orphans))
	}
	code := 0
	if rep.Blocked != "" {
		_, _ = fmt.Fprintf(stderr, "sweep-orphans: deletion refused: %s\n", rep.Blocked)
		code = exitFailure
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "sweep-orphans: %v\n", err)
		code = exitFailure
	}
	return code
}
