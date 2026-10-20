package tasks_test

import (
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentcontext"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// maxGuidanceBytes bounds what the brief adds around the delegator's text, so the guidance cannot
// grow into the 16 KiB the assigned work may use.
const maxGuidanceBytes = 3072

// TestGuidanceDoesNotKeyTheCheckoutOnTheRole holds the wording to what is true of every brief: the
// checkout belongs to code tasks, whatever their role, and the guidance names no role as lacking it.
func TestGuidanceDoesNotKeyTheCheckoutOnTheRole(t *testing.T) {
	for _, bad := range []string{"main and supervisor sessions have no checkout", "supervisor sessions have no checkout"} {
		if strings.Contains(agentcontext.Guidance, bad) {
			t.Errorf("guidance says %q", bad)
		}
	}
	if want := "Code tasks run in an isolated workspace"; !strings.Contains(agentcontext.Guidance, want) {
		t.Errorf("guidance lacks %q", want)
	}
}

func TestBriefsStateTheRoleAndShareTheGuidance(t *testing.T) {
	const work = "Ship the thing"
	cases := []struct {
		name string
		role dbstate.AttemptRole
		mode dbstate.TaskMode
		want []string
		not  []string
	}{
		{
			name: "supervisor/coordination", role: dbstate.AttemptSupervisor, mode: dbstate.TaskModeCoordination,
			want: []string{"You are the Mainloop task supervisor.", "no repository authority"},
			not:  []string{"## Workspace environment", "open_pull_request", "dev_sync"},
		},
		{
			// A root code task is a supervisor by position and has the checkout.
			name: "supervisor/code", role: dbstate.AttemptSupervisor, mode: dbstate.TaskModeCode,
			want: []string{
				"You are the Mainloop task supervisor.", "open_pull_request", "## Workspace environment",
				"gVisor sandbox", "SSL_CERT_FILE", "dev_sync", "empty commit",
			},
			not: []string{"no repository authority"},
		},
		{
			name: "child/code", role: dbstate.AttemptChild, mode: dbstate.TaskModeCode,
			want: []string{
				"You are the Mainloop task child.", "open_pull_request", "## Workspace environment",
				"gVisor sandbox", "SSL_CERT_FILE", "dev_sync", "empty commit",
			},
		},
		{
			name: "child/coordination", role: dbstate.AttemptChild, mode: dbstate.TaskModeCoordination,
			want: []string{"You are the Mainloop task child.", "no repository authority"},
			not:  []string{"## Workspace environment", "open_pull_request", "dev_sync"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tasks.DelegatedBrief(c.role, c.mode, work)
			if !strings.Contains(got, "## Assigned work\n"+work+"\n\n") {
				t.Fatalf("the assigned work is not intact in %q", got)
			}
			if !strings.Contains(got, agentcontext.Guidance) {
				t.Errorf("the shared guidance is missing:\n%s", got)
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("brief lacks %q:\n%s", w, got)
				}
			}
			for _, w := range c.not {
				if strings.Contains(got, w) {
					t.Errorf("brief has %q:\n%s", w, got)
				}
			}
			if n := len(got) - len(work); n > maxGuidanceBytes {
				t.Errorf("guidance is %d bytes, want at most %d", n, maxGuidanceBytes)
			}
		})
	}
}
