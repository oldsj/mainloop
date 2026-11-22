package mcpserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
	"github.com/oldsj/mainloop/server/internal/onboarding"
)

type notInstalled struct{}

func (notInstalled) Installed(context.Context, github.Repo) (bool, error) { return false, nil }

// TestProjectProposeOpensACardForTheOwner: main's proposal comes back with its id, its card in Needs
// you and the owner's next step when the App is missing; a supervisor's proposal records its task;
// a replay reads the same proposal back; a bad argument is a validation error.
func TestProjectProposeOpensACardForTheOwner(t *testing.T) {
	h := buildHarness(t, false, true, func(s *mcpserver.Server, pool *pgxpool.Pool) {
		s.Onboarding = &onboarding.Service{Pool: pool, GitHub: notInstalled{}}
	})
	ctx := context.Background()
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
	task, _ := h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptActive, sup)

	args := map[string]any{
		"request_id": "r1", "repository": "oldsj/opencozy-next", "why": "pilot",
		"verification_mode": "ci", "services": []string{"postgres:16"}, "workspace_size": "large",
	}
	_, r := h.call(mcpserver.PathOrdinary, main.Token, "project_propose", args)
	sc := r.Result.StructuredContent
	if r.Result.IsError || sc["state"] != "proposed" || sc["repository"] != "oldsj/opencozy-next" ||
		sc["github_app"] != "not_installed" || sc["replayed"] != false ||
		!strings.Contains(r.text(), "NOT installed on the repository; tell the owner: Install Mainloop's GitHub App") ||
		!strings.Contains(r.text(), "recorded on the card, not applied") {
		t.Fatalf("propose = %v %q", sc, r.text())
	}
	id := sc["proposal_id"].(string)
	if sc["inbox_url"] != "/inbox?item=project_proposal_"+id {
		t.Errorf("inbox_url = %v", sc["inbox_url"])
	}
	var who string
	if err := h.env.Pool.QueryRow(ctx, `SELECT concat_ws('|', proposer_role, proposer_session_id) FROM project_proposals WHERE id = $1`, id).Scan(&who); err != nil || who != "main|"+main.SessionID {
		t.Fatalf("proposer = %q, %v", who, err)
	}

	_, r = h.call(mcpserver.PathOrdinary, main.Token, "project_propose", args)
	if r.Result.IsError || r.Result.StructuredContent["proposal_id"] != id || r.Result.StructuredContent["replayed"] != true {
		t.Fatalf("replay = %v", r.Result.StructuredContent)
	}

	// Once applied, the same request id reads the outcome back.
	if _, err := h.env.Pool.Exec(ctx, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url)
		VALUES ('p-ocn', 'owner', 'oldsj', 'opencozy-next', 'oldsj/opencozy-next', 'https://x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.env.Pool.Exec(ctx, `UPDATE project_proposals SET state = 'applied', decided_at = now(), finished_at = now(),
		project_id = 'p-ocn', attempts = 1 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	_, r = h.call(mcpserver.PathOrdinary, main.Token, "project_propose", args)
	if sc := r.Result.StructuredContent; r.Result.IsError || sc["state"] != "applied" || sc["project_id"] != "p-ocn" ||
		!strings.Contains(r.text(), "was approved and applied: project p-ocn") {
		t.Fatalf("applied replay = %v %q", sc, r.text())
	}

	_, r = h.call(mcpserver.PathOrdinary, sup.Token, "project_propose", map[string]any{"request_id": "r2", "repository": "oldsj/other", "why": "x"})
	if r.Result.IsError {
		t.Fatalf("supervisor propose = %q", r.text())
	}
	if err := h.env.Pool.QueryRow(ctx, `SELECT concat_ws('|', proposer_role, proposer_task_id) FROM project_proposals WHERE request_id = 'r2'`).Scan(&who); err != nil || who != "supervisor|"+task {
		t.Fatalf("supervisor proposer = %q, %v", who, err)
	}

	_, r = h.call(mcpserver.PathOrdinary, main.Token, "project_propose", map[string]any{"request_id": "r3", "repository": "oldsj/x", "why": "x", "verification_mode": "sometimes"})
	if !r.Result.IsError || !strings.Contains(r.text(), "[validation]") {
		t.Fatalf("bad mode = %q", r.text())
	}
	_, r = h.call(mcpserver.PathOrdinary, main.Token, "project_propose", map[string]any{"request_id": "r1", "repository": "oldsj/opencozy-next", "why": "changed"})
	if !r.Result.IsError || !strings.Contains(r.text(), "[conflict]") {
		t.Fatalf("changed replay = %q", r.text())
	}
}

// TestProjectProposeUnavailableWithoutAService: a process built without proposals says so.
func TestProjectProposeUnavailableWithoutAService(t *testing.T) {
	h := buildHarness(t, false, true, func(s *mcpserver.Server, _ *pgxpool.Pool) { s.Onboarding = nil })
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	_, r := h.call(mcpserver.PathOrdinary, main.Token, "project_propose", map[string]any{"request_id": "r1", "repository": "o/r", "why": "x"})
	if !r.Result.IsError || !strings.Contains(r.text(), "[unavailable]") {
		t.Fatalf("propose = %q", r.text())
	}
}
