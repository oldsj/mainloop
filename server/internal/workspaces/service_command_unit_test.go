package workspaces

import (
	"slices"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
)

func TestServiceCommandEnvironment(t *testing.T) {
	scenarios := []struct {
		name     string
		declared []string
		ports    []api.WorkspacePort
		want     []string
	}{
		{
			"Given a declared command and two ports, when the command is planned, assert that env sets the workspace id and each port's preview URL in port order",
			[]string{"scripts/dev/workspace.sh", "run"},
			[]api.WorkspacePort{{Name: "api", Number: 8080}, {Name: "web", Number: 3000}},
			[]string{
				"env", "MAINLOOP_WORKSPACE_ID=ws-1",
				"MAINLOOP_PREVIEW_URLS=3000=https://3000--ws-1--preview.preview.example,8080=https://8080--ws-1--preview.preview.example",
				"scripts/dev/workspace.sh", "run",
			},
		},
		{
			"Given a declared command and no ports, when the command is planned, assert that the preview URLs are empty",
			[]string{"make", "serve"}, nil,
			[]string{"env", "MAINLOOP_WORKSPACE_ID=ws-1", "MAINLOOP_PREVIEW_URLS=", "make", "serve"},
		},
		{
			"Given no declared command, when the command is planned, assert that none is sent",
			nil, []api.WorkspacePort{{Name: "web", Number: 3000}}, []string{},
		},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			got, err := serviceCommand(s.declared, "https://preview.example", "ws-1", s.ports)

			if err != nil || !slices.Equal(got, s.want) || got == nil {
				t.Fatalf("serviceCommand() = %q, %v; want %q", got, err, s.want)
			}
		})
	}

	t.Run("Given ports whose preview URLs exceed kagent's argument size, when the command is planned, assert that it is refused", func(t *testing.T) {
		ports := make([]api.WorkspacePort, 0, 80)
		for n := range 80 {
			ports = append(ports, api.WorkspacePort{Name: "p", Number: 10000 + n})
		}

		_, err := serviceCommand([]string{"make"}, "https://"+strings.Repeat("a", 20)+".example", "ws-1", ports)

		if err == nil {
			t.Fatal("serviceCommand() error = nil, want the size limit")
		}
	})
}
