package workspaces

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
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

func TestWorkspaceServicePorts(t *testing.T) {
	web := []api.WorkspacePort{{Name: "web", Number: 3000}, {Name: "api", Number: 8080}}

	t.Run("Given a project that declares preview ports, when a workspace is created without ports of its own, as a task's is, assert that it declares the project's ports, lists them in MAINLOOP_PREVIEW_URLS, and keeps them after the project changes", func(t *testing.T) {
		h := newHarness(t, nil)
		project, err := h.svc.Projects.Import(bg, "oldsj/mainloop")
		if err != nil {
			t.Fatal(err)
		}
		h.setService(project.ID, []string{"npm", "run", "dev"}, web)

		w := h.createRunning(api.CreateWorkspaceRequest{ProjectId: &project.ID})
		h.setService(project.ID, []string{"npm", "run", "dev"}, nil)

		if got := h.row(w.WorkspaceId).ports(); !slices.Equal(got, web) {
			t.Fatalf("workspace ports = %v, want the project's %v", got, web)
		}
		urls := "MAINLOOP_PREVIEW_URLS=3000=" + previewURL("http://localhost:8001", w.WorkspaceId, 3000) +
			",8080=" + previewURL("http://localhost:8001", w.WorkspaceId, 8080)
		want := []string{"env", "MAINLOOP_WORKSPACE_ID=" + w.WorkspaceId, urls, "npm", "run", "dev"}
		if got := h.row(w.WorkspaceId).createRequest().GetWorkspace().GetServiceCommand(); !slices.Equal(got, want) {
			t.Fatalf("service command = %q, want %q", got, want)
		}
	})

	t.Run("Given a project that declares preview ports, when a workspace is created with its own dev.ports, assert that the request's ports win, an empty list included", func(t *testing.T) {
		for _, own := range [][]api.WorkspacePort{{{Name: "docs", Number: 4000}}, {}} {
			h := newHarness(t, nil)
			project, err := h.svc.Projects.Import(bg, "oldsj/mainloop")
			if err != nil {
				t.Fatal(err)
			}
			h.setService(project.ID, nil, web)

			w := h.createRunning(api.CreateWorkspaceRequest{ProjectId: &project.ID, Dev: &api.WorkspaceDev{Ports: &own}})

			if got := h.row(w.WorkspaceId).ports(); !slices.Equal(got, own) {
				t.Fatalf("workspace ports = %v, want the request's %v", got, own)
			}
		}
	})
}

func (h *harness) createRunning(body api.CreateWorkspaceRequest) api.WorkspaceLifecycle {
	h.t.Helper()
	stop := h.run()
	defer stop()
	defer h.patient(15 * time.Second)()
	w, err := h.create(body)
	if err != nil {
		h.t.Fatal(err)
	}
	return w
}

func (h *harness) setServiceCommand(projectID string, command []string) {
	h.t.Helper()
	h.setService(projectID, command, nil)
}

func (h *harness) setService(projectID string, command []string, ports []api.WorkspacePort) {
	h.t.Helper()
	if command == nil {
		command = []string{}
	}
	if ports == nil {
		ports = []api.WorkspacePort{}
	}
	portJSON, err := json.Marshal(ports)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.dq.SetOwnedProjectWorkspaceService(bg, dbq.SetOwnedProjectWorkspaceServiceParams{
		ID: projectID, UserID: owner, WorkspaceServiceCommand: command, WorkspaceServicePorts: portJSON,
	}); err != nil {
		h.t.Fatal(err)
	}
}
