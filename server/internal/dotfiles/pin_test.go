package dotfiles_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/devloop/devlooptest"
	"github.com/oldsj/mainloop/server/internal/dotfiles"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

// countingRefs is a RefResolver that names the same commit for every ref and counts the calls.
type countingRefs struct {
	sha   string
	err   error
	calls int
	refs  []string
}

func (r *countingRefs) CommitSHA(_ context.Context, _ github.Repo, ref string) (string, error) {
	r.calls++
	r.refs = append(r.refs, ref)
	return r.sha, r.err
}

type pinEnv struct {
	pool   *pgxpool.Pool
	q      *dbq.Queries
	refs   *countingRefs
	seeder *devlooptest.Seeder
	e      *dotfiles.Exports
}

func newPinEnv(t *testing.T) *pinEnv {
	t.Helper()
	d := testdb.New(t)
	ctx := context.Background()
	sha, pack := fixtureRepo(t, fixture)
	env := &pinEnv{pool: d.Pool, q: dbq.New(d.Pool), refs: &countingRefs{sha: sha}, seeder: &devlooptest.Seeder{Payload: pack}}
	env.e = &dotfiles.Exports{Pool: d.Pool, Refs: env.refs, Seeder: env.seeder}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url)
		VALUES ('p1', $1, 'o', 'n', 'o/n', 'https://example.test/o/n')`, owner); err != nil {
		t.Fatal(err)
	}
	project := "p1"
	for _, s := range []dbq.CreateSessionParams{
		{ID: "main", UserID: owner, Title: "main thread"},
		{ID: "task", UserID: owner, ProjectID: &project, Title: "task"},
	} {
		if _, err := env.q.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

func (env *pinEnv) setOwner(t *testing.T, repo, ref, path string) {
	t.Helper()
	if _, err := env.q.InsertOwnerDotfilesSource(context.Background(), dbq.InsertOwnerDotfilesSourceParams{
		OwnerID: owner, Repository: &repo, Ref: ref, Path: path,
	}); err != nil {
		t.Fatal(err)
	}
}

func (env *pinEnv) pin(t *testing.T, session, request string) dotfiles.Pinned {
	t.Helper()
	p, err := env.e.Pin(context.Background(), session, request)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPinWithNoSource(t *testing.T) {
	env := newPinEnv(t)
	if p := env.pin(t, "main", "req-1"); p != (dotfiles.Pinned{Status: dotfiles.StatusNone}) {
		t.Fatalf("pin = %+v", p)
	}
	if env.refs.calls != 0 {
		t.Fatalf("resolved %d times with no source", env.refs.calls)
	}
}

func TestPinIsKeptPerRequestAndRenewedForANewSession(t *testing.T) {
	env := newPinEnv(t)
	ctx := context.Background()
	env.setOwner(t, "example/dotfiles", "main", "home/.agents")

	first := env.pin(t, "main", "req-1")
	if first.Status != dotfiles.StatusReady || first.Commit != env.refs.sha || first.Digest == "" ||
		first.Repository != "example/dotfiles" || first.Ref != "main" || first.Path != "home/.agents" {
		t.Fatalf("pin = %+v", first)
	}

	// A retried create of the same request is given the same pin without asking GitHub.
	if again := env.pin(t, "main", "req-1"); again != first || env.refs.calls != 1 {
		t.Fatalf("retry pin = %+v after %d resolves", again, env.refs.calls)
	}

	// A new Session of the same Mainloop session (a provider switch) resolves the ref again.
	if next := env.pin(t, "main", "req-2"); next.Digest != first.Digest || env.refs.calls != 2 {
		t.Fatalf("new request pin = %+v after %d resolves", next, env.refs.calls)
	}
	if env.seeder.Calls != 1 {
		t.Fatalf("fetched %d times, want the commit's export reused", env.seeder.Calls)
	}

	latest, ok, err := dotfiles.Latest(ctx, env.q, "main")
	if err != nil || !ok || latest != first {
		t.Fatalf("latest = %+v, %v, %v", latest, ok, err)
	}
}

func TestPinPrefersTheProjectSource(t *testing.T) {
	env := newPinEnv(t)
	ctx := context.Background()
	env.setOwner(t, "example/dotfiles", "", "home/.agents")

	// A project source row without a repository falls back to the owner's.
	if _, err := env.q.InsertProjectDotfilesSource(ctx, dbq.InsertProjectDotfilesSourceParams{ProjectID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if p := env.pin(t, "task", "req-a"); p.Repository != "example/dotfiles" || p.Path != "home/.agents" {
		t.Fatalf("fallback pin = %+v", p)
	}
	if env.refs.refs[0] != "HEAD" {
		t.Fatalf("resolved %q, want the default branch", env.refs.refs[0])
	}

	repo := "example/project-agents"
	if _, err := env.q.UpdateProjectDotfilesSource(ctx, dbq.UpdateProjectDotfilesSourceParams{
		ProjectID: "p1", Repository: &repo, Ref: "v1", Path: "", ExpectedVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	p := env.pin(t, "task", "req-b")
	if p.Repository != repo || p.Ref != "v1" || p.Path != "" {
		t.Fatalf("project pin = %+v", p)
	}
	// The fixture keeps its files under home/.agents, so the repository root holds nothing to export.
	if p.Status != dotfiles.StatusRefused || p.Reason != dotfiles.ReasonNothingToExport || p.Commit != env.refs.sha {
		t.Fatalf("project pin = %+v", p)
	}
	// The main thread has no project: it keeps the owner's source.
	if m := env.pin(t, "main", "req-c"); m.Repository != "example/dotfiles" {
		t.Fatalf("main pin = %+v", m)
	}
}

func TestPinRecordsAnUnavailableSource(t *testing.T) {
	env := newPinEnv(t)
	env.setOwner(t, "example/dotfiles", "gone", "")
	env.refs.err = github.ErrNotFound
	p := env.pin(t, "main", "req-1")
	want := dotfiles.Pinned{Repository: "example/dotfiles", Ref: "gone", Status: dotfiles.StatusUnavailable, Reason: dotfiles.ReasonRefNotFound}
	if p != want {
		t.Fatalf("pin = %+v, want %+v", p, want)
	}
	// The request keeps that answer, so a replay sends what the first attempt sent.
	env.refs.err = nil
	if again := env.pin(t, "main", "req-1"); again != want {
		t.Fatalf("retry pin = %+v", again)
	}
}

func TestForkCopiesTheSourcePin(t *testing.T) {
	env := newPinEnv(t)
	ctx := context.Background()
	env.setOwner(t, "example/dotfiles", "main", "home/.agents")
	src := env.pin(t, "task", "req-1")
	if _, err := env.q.CreateSession(ctx, dbq.CreateSessionParams{ID: "fork", UserID: owner, Title: "fork"}); err != nil {
		t.Fatal(err)
	}
	if err := env.q.CopyForkDotfiles(ctx, dbq.CopyForkDotfilesParams{
		RequestID: "fork:f1", SessionID: "fork", SourceSessionID: "task",
	}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := dotfiles.Latest(ctx, env.q, "fork")
	if err != nil || !ok || got != src {
		t.Fatalf("fork pin = %+v, %v, %v; want %+v", got, ok, err, src)
	}
	if env.refs.calls != 1 {
		t.Fatalf("a fork resolved the ref again (%d calls)", env.refs.calls)
	}
}
