package workspaces

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// importSpy stands in for projects.Service.Import and records what it was asked for.
type importSpy struct {
	h     *harness
	mu    sync.Mutex
	texts []string
	// defaultBranch is what the imported project reports.
	defaultBranch string
}

func (i *importSpy) Import(ctx context.Context, text string) (dbq.Project, error) {
	i.mu.Lock()
	i.texts = append(i.texts, text)
	i.mu.Unlock()
	return dbq.New(i.h.svc.Pool).ImportProject(ctx, dbq.ImportProjectParams{
		ID: uuid.NewString(), UserID: owner, Owner: "oldsj", Name: "spied", FullName: "oldsj/spied",
		DefaultBranch: i.defaultBranch, HTMLURL: "https://github.com/oldsj/spied",
	})
}

func (h *harness) baseBranch(id string) string {
	h.t.Helper()
	var b string
	if err := h.svc.Pool.QueryRow(bg, `SELECT base_branch FROM sessions WHERE id = $1`, id).Scan(&b); err != nil {
		h.t.Fatal(err)
	}
	return b
}

// A workspace requested by repository creates its project through the project service, the one
// way a project is made, and takes the project's default branch as its base.
func TestWorkspaceFromARepoImportsThroughTheProjectService(t *testing.T) {
	h := newHarness(t, nil)
	h.run()
	spy := &importSpy{h: h, defaultBranch: "trunk"}
	h.svc.Projects = spy
	w, err := h.create(api.CreateWorkspaceRequest{Repo: ptr("OldSJ/Spied")})
	if err != nil {
		t.Fatal(err)
	}
	if len(spy.texts) != 1 || spy.texts[0] != "OldSJ/Spied" {
		t.Fatalf("imports = %v", spy.texts)
	}
	if got := h.baseBranch(w.WorkspaceId); got != "trunk" {
		t.Fatalf("base_branch = %q", got)
	}
}

// With no importer configured the workspace service still imports through a project service, and
// a project with no default branch yet gives an empty base: the remote's default, not "main".
func TestEmptyRefLeavesBaseBranchEmpty(t *testing.T) {
	h := newHarness(t, nil)
	h.run()
	w, err := h.create(api.CreateWorkspaceRequest{Repo: ptr("oldsj/mainloop")})
	if err != nil {
		t.Fatal(err)
	}
	var branch string
	if err := h.svc.Pool.QueryRow(bg, `SELECT default_branch FROM projects WHERE full_name = 'oldsj/mainloop'`).Scan(&branch); err != nil || branch != "" {
		t.Fatalf("project default branch %q: %v", branch, err)
	}
	if got := h.baseBranch(w.WorkspaceId); got != "" {
		t.Fatalf("base_branch = %q, want empty", got)
	}
	if h.row(w.WorkspaceId).Ref != "" {
		t.Fatalf("workspace ref = %q", h.row(w.WorkspaceId).Ref)
	}
	explicit, err := h.create(api.CreateWorkspaceRequest{Repo: ptr("oldsj/mainloop"), Ref: ptr("release")})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.baseBranch(explicit.WorkspaceId); got != "release" {
		t.Fatalf("base_branch = %q", got)
	}
}

func TestInvalidRepoIsStillInvalid(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.create(api.CreateWorkspaceRequest{Repo: ptr("https://gitlab.com/a/b")})
	var inv *InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("err = %v", err)
	}
	if h.workspaceCount() != 0 {
		t.Fatal("a rejected request left a workspace")
	}
}
