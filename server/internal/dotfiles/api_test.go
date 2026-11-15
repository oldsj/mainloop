package dotfiles_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/dotfiles"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const owner = "owner-1"

func newAPI(t *testing.T) *dotfiles.DotfilesAPI {
	t.Helper()
	d := testdb.New(t)
	ctx := context.Background()
	for _, p := range []struct{ id, user string }{{"p1", owner}, {"p-other", "someone-else"}} {
		if _, err := d.Pool.Exec(ctx, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url)
			VALUES ($1, $2, 'o', 'n', 'o/n', 'https://example.test/o/n')`, p.id, p.user); err != nil {
			t.Fatal(err)
		}
	}
	return dotfiles.NewAPI(d.Pool, owner)
}

func ptr(s string) *string { return &s }

func status(err error) int {
	var e *apierr.Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func TestOwnerSourceSetReadAndClear(t *testing.T) {
	a, ctx := newAPI(t), context.Background()

	got, err := a.GetDotfilesSource(ctx, api.GetDotfilesSourceRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	if src := api.DotfilesSource(got.(api.GetDotfilesSource200JSONResponse)); src.Repository != nil || src.Version != 0 {
		t.Fatalf("unset source = %+v, want repository null at version 0", src)
	}

	set, err := a.SetDotfilesSource(ctx, api.SetDotfilesSourceRequestObject{Body: &api.DotfilesSourceUpdate{
		Repository: ptr("example/agent-dotfiles"), Ref: ptr("main"), Path: ptr("home/.agents/"), ExpectedVersion: 0,
	}})
	if err != nil {
		t.Fatal(err)
	}
	src := api.DotfilesSource(set.(api.SetDotfilesSource200JSONResponse))
	if *src.Repository != "example/agent-dotfiles" || src.Ref != "main" || src.Path != "home/.agents" || src.Version != 1 {
		t.Fatalf("set source = %+v", src)
	}

	// A stale version is a conflict, and changes nothing.
	_, err = a.SetDotfilesSource(ctx, api.SetDotfilesSourceRequestObject{Body: &api.DotfilesSourceUpdate{
		Repository: ptr("example/other"), ExpectedVersion: 0,
	}})
	if status(err) != http.StatusConflict {
		t.Fatalf("stale insert: err = %v, want 409", err)
	}

	// Clearing drops ref and path with the repository.
	cleared, err := a.SetDotfilesSource(ctx, api.SetDotfilesSourceRequestObject{Body: &api.DotfilesSourceUpdate{
		Repository: nil, Ref: ptr("main"), Path: ptr("x"), ExpectedVersion: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if src := api.DotfilesSource(cleared.(api.SetDotfilesSource200JSONResponse)); src.Repository != nil || src.Ref != "" || src.Path != "" || src.Version != 2 {
		t.Fatalf("cleared source = %+v", src)
	}
}

func TestProjectSourceIsTheOwnersProjectsOnly(t *testing.T) {
	a, ctx := newAPI(t), context.Background()

	_, err := a.GetProjectDotfilesSource(ctx, api.GetProjectDotfilesSourceRequestObject{ProjectId: "p-other"})
	if status(err) != http.StatusNotFound {
		t.Fatalf("another owner's project: err = %v, want 404", err)
	}
	_, err = a.SetProjectDotfilesSource(ctx, api.SetProjectDotfilesSourceRequestObject{ProjectId: "missing",
		Body: &api.DotfilesSourceUpdate{Repository: ptr("example/agent-dotfiles"), ExpectedVersion: 0}})
	if status(err) != http.StatusNotFound {
		t.Fatalf("missing project: err = %v, want 404", err)
	}

	set, err := a.SetProjectDotfilesSource(ctx, api.SetProjectDotfilesSourceRequestObject{ProjectId: "p1",
		Body: &api.DotfilesSourceUpdate{Repository: ptr("example/agent-dotfiles"), Ref: ptr("project-x-stable"), ExpectedVersion: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if src := api.DotfilesSource(set.(api.SetProjectDotfilesSource200JSONResponse)); src.Ref != "project-x-stable" || src.Version != 1 {
		t.Fatalf("project source = %+v", src)
	}
	got, err := a.GetProjectDotfilesSource(ctx, api.GetProjectDotfilesSourceRequestObject{ProjectId: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if src := api.DotfilesSource(got.(api.GetProjectDotfilesSource200JSONResponse)); *src.Repository != "example/agent-dotfiles" {
		t.Fatalf("read back = %+v", src)
	}
	_, err = a.SetProjectDotfilesSource(ctx, api.SetProjectDotfilesSourceRequestObject{ProjectId: "p1",
		Body: &api.DotfilesSourceUpdate{Repository: nil, ExpectedVersion: 7}})
	if status(err) != http.StatusConflict {
		t.Fatalf("stale update: err = %v, want 409", err)
	}

	// The owner-wide source is untouched by a project's.
	owner, err := a.GetDotfilesSource(ctx, api.GetDotfilesSourceRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	if src := api.DotfilesSource(owner.(api.GetDotfilesSource200JSONResponse)); src.Repository != nil {
		t.Fatalf("owner-wide source = %+v, want none", src)
	}
}

func TestInvalidSourcesAreRefused(t *testing.T) {
	a, ctx := newAPI(t), context.Background()
	for name, body := range map[string]api.DotfilesSourceUpdate{
		"full ref path":  {Repository: ptr("example/d"), Ref: ptr("refs/heads/main")},
		"bad ref":        {Repository: ptr("example/d"), Ref: ptr("a..b")},
		"absolute path":  {Repository: ptr("example/d"), Path: ptr("/etc")},
		"parent segment": {Repository: ptr("example/d"), Path: ptr("home/../..")},
		"dot segment":    {Repository: ptr("example/d"), Path: ptr("./home")},
		"empty segment":  {Repository: ptr("example/d"), Path: ptr("home//x")},
		"backslash":      {Repository: ptr("example/d"), Path: ptr(`home\x`)},
		"control char":   {Repository: ptr("example/d"), Path: ptr("home\tx")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := a.SetDotfilesSource(ctx, api.SetDotfilesSourceRequestObject{Body: &body})
			if status(err) != http.StatusUnprocessableEntity {
				t.Fatalf("err = %v, want 422", err)
			}
		})
	}
}
