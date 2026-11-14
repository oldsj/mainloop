package workspaces

import (
	"slices"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

func TestWorkspaceServiceCommand(t *testing.T) {
	t.Run("Given a project that declares a workspace service, when a workspace is created for it, assert that kagent is asked to run the command with the workspace's preview environment and a replay keeps it after the project changes", func(t *testing.T) {
		h := newHarness(t, nil)
		project, err := h.svc.Projects.Import(bg, "oldsj/mainloop")
		if err != nil {
			t.Fatal(err)
		}
		declared := []string{"npm", "run", "dev"}
		h.setServiceCommand(project.ID, declared)

		w := h.running()
		h.setServiceCommand(project.ID, []string{"make", "serve"})

		creates := h.fake.CreateRequests()
		want := []string{"env", "MAINLOOP_WORKSPACE_ID=" + w.WorkspaceId, "MAINLOOP_PREVIEW_URLS=", "npm", "run", "dev"}
		if len(creates) != 1 || !slices.Equal(creates[0].GetWorkspace().GetServiceCommand(), want) {
			t.Fatalf("kagent create requests = %v, want one with service command %q", creates, want)
		}
		if got := h.row(w.WorkspaceId).createRequest().GetWorkspace().GetServiceCommand(); !slices.Equal(got, want) {
			t.Fatalf("replayed service command = %q, want the one the workspace was created with, %q", got, want)
		}
	})

	t.Run("Given a project without a workspace service, when a workspace is created for it, assert that kagent is asked to run nothing", func(t *testing.T) {
		h := newHarness(t, nil)

		h.running()

		creates := h.fake.CreateRequests()
		if len(creates) != 1 || len(creates[0].GetWorkspace().GetServiceCommand()) != 0 {
			t.Fatalf("kagent create requests = %v, want one without a service command", creates)
		}
	})
}

func (h *harness) setServiceCommand(projectID string, command []string) {
	h.t.Helper()
	if _, err := h.dq.SetOwnedProjectWorkspaceService(bg, dbq.SetOwnedProjectWorkspaceServiceParams{
		ID: projectID, UserID: owner, WorkspaceServiceCommand: command,
	}); err != nil {
		h.t.Fatal(err)
	}
}
