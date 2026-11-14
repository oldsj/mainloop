package app_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
)

func TestProjectWorkspaceService(t *testing.T) {
	t.Run("Given an imported project, when the owner sets and then clears its workspace service, assert that each read returns the stored command", func(t *testing.T) {
		h, _ := projectServer(t, false)
		p := importProject(t, h, "oldsj/mainloop")
		path := "/projects/" + p.Id + "/workspace-service"
		var v api.WorkspaceService

		r := send(h, "GET", path, "", nil)
		strict(t, r.body, &v)
		if r.status != 200 || v.Command == nil || len(v.Command) != 0 || v.Ports == nil || len(v.Ports) != 0 {
			t.Fatalf("initial: %d %s, want an empty command and no ports", r.status, r.body)
		}
		r = send(h, "PUT", path, `{"command":["npm","run","dev","--","--host","0.0.0.0"],"ports":[{"name":"web","number":3000}]}`, nil)
		strict(t, r.body, &v)
		want := []string{"npm", "run", "dev", "--", "--host", "0.0.0.0"}
		wantPorts := []api.WorkspacePort{{Name: "web", Number: 3000}}
		if r.status != 200 || !slices.Equal(v.Command, want) || !slices.Equal(v.Ports, wantPorts) {
			t.Fatalf("put: %d %s", r.status, r.body)
		}
		r = send(h, "GET", path, "", nil)
		strict(t, r.body, &v)
		if !slices.Equal(v.Command, want) || !slices.Equal(v.Ports, wantPorts) {
			t.Fatalf("read back: %s", r.body)
		}
		r = send(h, "PUT", path, `{"command":[],"ports":[]}`, nil)
		strict(t, r.body, &v)
		if r.status != 200 || len(v.Command) != 0 || v.Ports == nil || len(v.Ports) != 0 {
			t.Fatalf("clear: %d %s", r.status, r.body)
		}
	})

	scenarios := []struct {
		name string
		body string
	}{
		{"Given an imported project, when the owner sends an empty argument, assert that it is refused", `{"command":["npm",""],"ports":[]}`},
		{"Given an imported project, when the owner sends more than 61 arguments, assert that it is refused", `{"command":[` + strings.Repeat(`"a",`, 61) + `"a"],"ports":[]}`},
		{"Given an imported project, when the owner sends an argument over 4096 bytes, assert that it is refused", `{"command":["` + strings.Repeat("a", 4097) + `"],"ports":[]}`},
		{"Given an imported project, when the owner omits the command, assert that it is refused", `{"ports":[]}`},
		{"Given an imported project, when the owner omits the ports, assert that it is refused", `{"command":["make"]}`},
		{"Given an imported project, when the owner sends a badly named port, assert that it is refused", `{"command":["make"],"ports":[{"name":"Web","number":3000}]}`},
		{"Given an imported project, when the owner sends port 0, assert that it is refused", `{"command":["make"],"ports":[{"name":"web","number":0}]}`},
		{"Given an imported project, when the owner sends two ports with one number, assert that it is refused", `{"command":["make"],"ports":[{"name":"web","number":3000},{"name":"api","number":3000}]}`},
		{"Given an imported project, when the owner sends two ports with one name, assert that it is refused", `{"command":["make"],"ports":[{"name":"web","number":3000},{"name":"web","number":3001}]}`},
		{"Given an imported project, when the owner sends 17 ports, assert that it is refused", `{"command":["make"],"ports":[` + seventeenPorts() + `]}`},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			h, _ := projectServer(t, false)
			p := importProject(t, h, "oldsj/mainloop")
			path := "/projects/" + p.Id + "/workspace-service"
			send(h, "PUT", path, `{"command":["make","serve"],"ports":[{"name":"web","number":8080}]}`, nil)

			r := send(h, "PUT", path, s.body, nil)

			if r.status != 422 {
				t.Fatalf("got %d %s, want 422", r.status, r.body)
			}
			var v api.WorkspaceService
			strict(t, send(h, "GET", path, "", nil).body, &v)
			if !slices.Equal(v.Command, []string{"make", "serve"}) || !slices.Equal(v.Ports, []api.WorkspacePort{{Name: "web", Number: 8080}}) {
				t.Fatalf("service after a refused write = %+v, want it unchanged", v)
			}
		})
	}

	t.Run("Given no such project, when the owner reads or writes its workspace service, assert that both are not found", func(t *testing.T) {
		h, _ := projectServer(t, false)

		get := send(h, "GET", "/projects/nope/workspace-service", "", nil)
		put := send(h, "PUT", "/projects/nope/workspace-service", `{"command":["make"],"ports":[]}`, nil)

		if get.status != 404 || put.status != 404 {
			t.Fatalf("get %d, put %d; want 404 for both", get.status, put.status)
		}
	})
}

func seventeenPorts() string {
	ports := make([]string, 17)
	for i := range ports {
		ports[i] = fmt.Sprintf(`{"name":"p%d","number":%d}`, i, 3000+i)
	}
	return strings.Join(ports, ",")
}
