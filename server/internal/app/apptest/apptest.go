// Package apptest builds [app.Deps] for tests through the services' own constructors, so a test
// wires services the way serve-api does and a constructor change cannot leave a test behind.
package apptest

import (
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/health"
	"github.com/oldsj/mainloop/server/internal/hitl"
	"github.com/oldsj/mainloop/server/internal/projects"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/schedules"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

type options struct {
	kagent   sessions.Kagent
	remote   projects.Remote
	observer *github.TaskObserver
}

// Option changes what [Deps] builds.
type Option func(*options)

// WithKagent gives the session service a kagent client. Without one it records sends and delivers
// none.
func WithKagent(k sessions.Kagent) Option { return func(o *options) { o.kagent = k } }

// WithRemote gives the project service a GitHub client. Without one import records the project
// alone.
func WithRemote(r projects.Remote) Option { return func(o *options) { o.remote = r } }

// WithTaskObserver connects task projection to GitHub. Without it nothing is projected.
func WithTaskObserver(obs *github.TaskObserver) Option { return func(o *options) { o.observer = obs } }

// Deps builds the services every API route needs over pool, which may be nil for a test that
// reaches no database. Services whose runtime a test does not provide (workspaces, merge,
// preview, devloop) are nil, so their routes answer 503; with a nil pool there are no tasks
// either. The caller sets the fields it needs (DB, Hub, Registry, Preview, Devloop) on the
// result.
func Deps(t testing.TB, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool, opts ...Option) app.Deps {
	t.Helper()
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	reg, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		t.Fatal(err)
	}
	attn := attention.NewService(cfg, pool, log)
	healthSvc := health.NewService(cfg, pool, nil, log)
	sessSvc := sessions.NewService(cfg, pool, o.kagent, reg, nil, healthSvc, nil, nil, log)
	d := app.Deps{
		Config: cfg, Log: log, Pool: pool,
		Providers: reg,
		Projects:  projects.NewService(cfg, pool, o.remote, nil, attn.NeedsOwner, log),
		Sessions:  sessSvc,
		HITL:      hitl.NewService(cfg, pool, nil, sessSvc, sessSvc, sessSvc, log),
		Attention: attn,
		Health:    healthSvc,
		Schedules: schedules.NewService(cfg, pool, nil, reg, log),
	}
	if pool != nil {
		var topts []tasks.Option
		if o.observer != nil {
			topts = append(topts, tasks.WithObserver(tasks.GitHubObserver(o.observer)))
		}
		d.Tasks = tasks.NewService(cfg, pool, reg, log, topts...)
	}
	return d
}
