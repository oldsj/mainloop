package main

import (
	"context"
	"fmt"
	"io"
	"os/signal"
	"syscall"
	"time"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
)

const migrateUsage = "usage: mainloop migrate up|status|reset\n"

// runMigrate is the migrate subcommand. It prints to stdout and stderr and returns the exit code.
func runMigrate(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprint(stderr, migrateUsage)
		return exitFailure
	}
	action := args[0]
	if action != "up" && action != "status" && action != "reset" {
		_, _ = fmt.Fprintf(stderr, "migrate: unknown action %q\n%s", action, migrateUsage)
		return exitFailure
	}
	cfg, err := config.LoadMigrate()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "migrate: %v\n", err)
		return exitFailure
	}
	if action == "reset" && !cfg.AllowReset {
		_, _ = fmt.Fprintln(stderr, "migrate reset: refused; it destroys all data. "+
			"Set MAINLOOP_ALLOW_RESET=1 on a disposable instance to allow it.")
		return exitFailure
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, migrateTimeout)
	defer cancel()

	m, err := db.NewMigrator(ctx, cfg.DB.URL())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "migrate: %v\n", err)
		return exitFailure
	}
	defer func() { _ = m.Close() }()

	switch action {
	case "up":
		n, err := m.Up(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "migrate: %v\n", err)
			return exitFailure
		}
		_, _ = fmt.Fprintf(stdout, "applied %d migration(s)\n", n)
	case "reset":
		if err := m.Reset(ctx); err != nil {
			_, _ = fmt.Fprintf(stderr, "migrate: %v\n", err)
			return exitFailure
		}
		_, _ = fmt.Fprintln(stdout, "schema reset and migrated to the latest version")
	case "status":
		rows, err := m.Status(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "migrate: %v\n", err)
			return exitFailure
		}
		for _, r := range rows {
			state := "pending"
			if r.Applied {
				state = "applied " + r.AppliedAt.UTC().Format(time.RFC3339)
			}
			_, _ = fmt.Fprintf(stdout, "%-24s %s\n", r.Name, state)
		}
	}
	return 0
}

// migrateTimeout bounds one migrate command, including the wait for another migrator's lock.
const migrateTimeout = 5 * time.Minute
