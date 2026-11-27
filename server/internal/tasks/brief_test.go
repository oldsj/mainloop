package tasks_test

import (
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentcontext"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// maxCodeGuidanceBytes and maxCoordinationGuidanceBytes bound what the brief adds around the
// delegator's text, so the guidance cannot grow into the 16 KiB the assigned work may use.
const (
	maxCodeGuidanceBytes         = 2880
	maxCoordinationGuidanceBytes = 1664
)

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
			not:  []string{"## Workspace environment", "open_pull_request", "dev_sync", "watch_checks", "get_verification", "/opt/cache"},
		},
		{
			// A root code task is a supervisor by position and has the checkout.
			name: "supervisor/code", role: dbstate.AttemptSupervisor, mode: dbstate.TaskModeCode,
			want: []string{
				"You are the Mainloop task supervisor.", "open_pull_request", "## Workspace environment",
				"sandbox", "SSL_CERT_FILE", "dev_sync", "staging target", "empty commit", "`get_verification`", "push once",
				"`watch_checks`", "end your turn", "three failed rounds", "starter workflow", "`/opt/cache`", "`GOCACHE`", "do not move them",
			},
			not: []string{"no repository authority"},
		},
		{
			name: "child/code", role: dbstate.AttemptChild, mode: dbstate.TaskModeCode,
			want: []string{
				"You are the Mainloop task child.", "open_pull_request", "## Workspace environment",
				"sandbox", "SSL_CERT_FILE", "dev_sync", "staging target", "empty commit", "`get_verification`", "push once",
				"`watch_checks`", "end your turn", "three failed rounds", "starter workflow", "`/opt/cache`", "`GOCACHE`", "do not move them",
			},
		},
		{
			name: "child/coordination", role: dbstate.AttemptChild, mode: dbstate.TaskModeCoordination,
			want: []string{"You are the Mainloop task child.", "no repository authority"},
			not:  []string{"## Workspace environment", "open_pull_request", "dev_sync", "watch_checks", "get_verification", "/opt/cache"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tasks.DelegatedBrief(c.role, c.mode, work, "")
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
			limit := maxCoordinationGuidanceBytes
			if c.mode == dbstate.TaskModeCode {
				limit = maxCodeGuidanceBytes
			}
			if n := len(got) - len(work); n > limit {
				t.Errorf("guidance is %d bytes, want at most %d", n, limit)
			}
		})
	}
}

// TestBriefCarriesProjectInstructionsBeforeTheGuidance: the owner's project instructions follow the
// assigned work and come before the shared guidance; a project without any adds no section.
func TestBriefCarriesProjectInstructionsBeforeTheGuidance(t *testing.T) {
	const work = "Ship the thing"
	section := projectcontext.Section(projectcontext.Instructions{ProjectID: "p1", Version: 3, Text: "UI demos use fictional data."})
	for _, mode := range []dbstate.TaskMode{dbstate.TaskModeCode, dbstate.TaskModeCoordination} {
		got := tasks.DelegatedBrief(dbstate.AttemptChild, mode, work, section)
		w, p, g := strings.Index(got, "## Assigned work"), strings.Index(got, "## Project instructions (owner, v3)"), strings.Index(got, agentcontext.Guidance)
		if w < 0 || p < 0 || g < 0 || w >= p || p >= g {
			t.Fatalf("%s: want assigned work, then project instructions, then guidance:\n%s", mode, got)
		}
		if !strings.Contains(got, "UI demos use fictional data.\n") {
			t.Errorf("%s: instructions text missing:\n%s", mode, got)
		}
	}
	if s := projectcontext.Section(projectcontext.Instructions{ProjectID: "p1"}); s != "" {
		t.Errorf("no instructions rendered %q", s)
	}
	if got := tasks.DelegatedBrief(dbstate.AttemptChild, dbstate.TaskModeCode, work, ""); strings.Contains(got, "Project instructions") {
		t.Errorf("a project without instructions got a section:\n%s", got)
	}
}
