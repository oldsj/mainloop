package workspaces

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/pushgate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
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
	s := &Service{
		Pool: pool, Kagent: client, Providers: reg, Config: cfg, Log: log, OwnerID: cfg.Identity.OwnerID,
	}
	if cfg.Git.TransportEnabled && cfg.Git.PushGateEnabled && cfg.GitHub.AppID != "" {
		upstream, err := pushgate.NewGitHubUpstream(github.Options{AppID: cfg.GitHub.AppID, PrivateKey: cfg.GitHub.AppPrivateKey.Reveal()})
		if err != nil {
			return nil, err
		}
		s.GitPush = &GitPush{Upstream: upstream, Origin: cfg.Git.PushOrigin}
	}
	return s, nil
}

// RunSweepOrphans lists the kagent Sessions Mainloop created and no longer references. It deletes
// nothing unless --delete is given. It first prints the Agents it sweeps. --forget-agent ns/name
// instead removes an Agent from that set (refused if the configuration names it).
func RunSweepOrphans(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sweep-orphans", flag.ContinueOnError)
	fs.SetOutput(stderr)
	del := fs.Bool("delete", false, "delete the orphans (default: list only)")
	forget := fs.String("forget-agent", "", "remove an Agent (namespace/name) from the swept set and exit")
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}
	if err := checkSweepFlags(fs, *del, *forget); err != nil {
		_, _ = fmt.Fprintf(stderr, "sweep-orphans: %v\n", err)
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
	if *forget != "" {
		return forgetAgent(ctx, svc, *forget, stdout, stderr)
	}
	if !*del {
		rep, err := svc.SweepOrphans(ctx, OrphanOptions{})
		return printOrphans(rep, err, false, stdout, stderr)
	}
	rep, ran, err := svc.deleteOrphansLeased(ctx)
	if err == nil && !ran {
		_, _ = fmt.Fprintln(stderr, "sweep-orphans: the scheduled orphan sweep is running; try again when it has finished")
		return exitFailure
	}
	return printOrphans(rep, err, true, stdout, stderr)
}

// deleteOrphansLeased is the one-shot deletion under the scheduled orphan sweep's lease, so the two
// never delete at the same time. ran is false, with no error, when the scheduled sweep holds it.
func (s *Service) deleteOrphansLeased(ctx context.Context) (rep OrphanReport, ran bool, err error) {
	ran, err = reconcile.NewRunner(s.Pool, s.Log, nil).RunSweep(ctx, reconcile.Sweep{Name: OrphanSweepName, Every: orphanTimeout, Timeout: orphanTimeout,
		Run: func(ctx context.Context) (err error) {
			rep, err = s.SweepOrphans(ctx, OrphanOptions{Delete: true})
			return err
		}})
	return rep, ran, err
}

// checkSweepFlags rejects a bad flag combination before the command touches the configuration or
// the database.
func checkSweepFlags(fs *flag.FlagSet, del bool, forget string) error {
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == "forget-agent" })
	switch {
	case given && forget == "":
		return errors.New("--forget-agent needs a namespace/name")
	case forget != "" && del:
		return errors.New("--forget-agent cannot be combined with --delete")
	}
	return nil
}

func forgetAgent(ctx context.Context, svc *Service, key string, stdout, stderr io.Writer) int {
	if err := svc.ForgetAgent(ctx, key); err != nil {
		_, _ = fmt.Fprintf(stderr, "sweep-orphans: %v\n", err)
		return exitFailure
	}
	_, _ = fmt.Fprintf(stdout, "forgot agent %s\n", key)
	return 0
}

func printOrphans(rep OrphanReport, err error, del bool, stdout, stderr io.Writer) int {
	for _, a := range rep.Agents {
		_, _ = fmt.Fprintf(stdout, "agent   %s\n", a)
	}
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
