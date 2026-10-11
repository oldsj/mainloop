package workspaces

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/httpx"
	"github.com/oldsj/mainloop/server/internal/kagent"
)

func TestCreateValidation(t *testing.T) {
	h := newHarness(t, nil)
	a := &WorkspacesAPI{Svc: h.svc}
	cases := map[string]api.CreateWorkspaceRequest{
		"both":      {ProjectId: ptr("p"), Repo: ptr("oldsj/mainloop")},
		"neither":   {},
		"port name": {Repo: ptr("oldsj/mainloop"), Dev: &api.WorkspaceDev{Ports: &[]api.WorkspacePort{{Name: "Bad Name", Number: 3000}}}},
		"depth":     {Repo: ptr("oldsj/mainloop"), Depth: ptr(5000)},
		"idle":      {Repo: ptr("oldsj/mainloop"), Dev: &api.WorkspaceDev{IdleTimeoutMinutes: ptr(0)}},
	}
	for name, body := range cases {
		res, err := a.CreateWorkspace(bg, api.CreateWorkspaceRequestObject{Body: &body})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := res.(api.CreateWorkspace422JSONResponse); !ok {
			t.Errorf("%s: %T", name, res)
		}
	}
	if h.workspaceCount() != 0 || h.fake.Count(kagent.ProcCreateSession) != 0 {
		t.Fatal("a rejected request left state behind")
	}
	res, err := a.CreateWorkspace(bg, api.CreateWorkspaceRequestObject{Body: &api.CreateWorkspaceRequest{ProjectId: ptr("nope")}})
	if _, ok := res.(api.CreateWorkspace404JSONResponse); !ok || err != nil {
		t.Fatalf("%T %v", res, err)
	}
}

func TestUnknownWorkspaceIs404(t *testing.T) {
	h := newHarness(t, nil)
	a := &WorkspacesAPI{Svc: h.svc}
	if res, _ := a.GetWorkspace(bg, api.GetWorkspaceRequestObject{WorkspaceId: "x"}); res == nil {
		t.Fatal("nil")
	} else if _, ok := res.(api.GetWorkspace404JSONResponse); !ok {
		t.Fatalf("%T", res)
	}
	if res, _ := a.DeleteWorkspace(bg, api.DeleteWorkspaceRequestObject{WorkspaceId: "x"}); res == nil {
		t.Fatal("nil")
	} else if _, ok := res.(api.DeleteWorkspace404JSONResponse); !ok {
		t.Fatalf("%T", res)
	}
	if res, _ := a.ListWorkspacePreviewPorts(bg, api.ListWorkspacePreviewPortsRequestObject{WorkspaceId: "x"}); res == nil {
		t.Fatal("nil")
	} else if _, ok := res.(api.ListWorkspacePreviewPorts404JSONResponse); !ok {
		t.Fatalf("%T", res)
	}
}

// The contract shapes: ports sorted by number with preview URLs, the lifecycle's JSON keys, the
// list, and delete as 204.
func TestContractShapes(t *testing.T) {
	h := newHarness(t, nil)
	a := &WorkspacesAPI{Svc: h.svc}
	stop := h.run()
	res, err := a.CreateWorkspace(bg, api.CreateWorkspaceRequestObject{Body: &api.CreateWorkspaceRequest{
		Repo: ptr("oldsj/mainloop"), Depth: ptr(1),
		Dev: &api.WorkspaceDev{IdleTimeoutMinutes: ptr(45), Ports: &[]api.WorkspacePort{{Name: "web", Number: 5173}, {Name: "api", Number: 3000}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	created, ok := res.(api.CreateWorkspace201JSONResponse)
	if !ok {
		t.Fatalf("%T", res)
	}
	id := created.WorkspaceId
	stop()

	raw, _ := json.Marshal(created)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"workspace_id", "session_id", "observed_state", "manifest", "publication_mode", "publication_reason", "updated_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in %s", k, raw)
		}
	}
	man := m["manifest"].(map[string]any)
	dev := man["dev"].(map[string]any)
	if man["depth"] != float64(1) || dev["idle_timeout_minutes"] != float64(45) || man["agent_kind"] != "claude" {
		t.Errorf("manifest %v", man)
	}

	pr, err := a.ListWorkspacePreviewPorts(bg, api.ListWorkspacePreviewPortsRequestObject{WorkspaceId: id})
	if err != nil {
		t.Fatal(err)
	}
	ports := pr.(api.ListWorkspacePreviewPorts200JSONResponse).Ports
	want := []api.WorkspacePreviewPort{
		{Name: "api", Port: 3000, Url: "http://3000--" + id + "--preview.localhost:8001"},
		{Name: "web", Port: 5173, Url: "http://5173--" + id + "--preview.localhost:8001"},
	}
	if len(ports) != 2 || ports[0] != want[0] || ports[1] != want[1] {
		t.Fatalf("ports %+v", ports)
	}

	lr, err := a.ListWorkspaces(bg, api.ListWorkspacesRequestObject{})
	if err != nil || len(lr.(api.ListWorkspaces200JSONResponse)) != 1 {
		t.Fatalf("%T %v", lr, err)
	}
	h.run()
	dr, err := a.DeleteWorkspace(bg, api.DeleteWorkspaceRequestObject{WorkspaceId: id})
	if _, ok := dr.(api.DeleteWorkspace204Response); !ok || err != nil {
		t.Fatalf("%T %v", dr, err)
	}
}

func TestNilServiceIs503(t *testing.T) {
	a := &WorkspacesAPI{}
	_, err := a.ListWorkspaces(bg, api.ListWorkspacesRequestObject{})
	var he *httpx.Error
	if !errors.As(err, &he) || he.Status != 503 {
		t.Fatalf("%v", err)
	}
}
