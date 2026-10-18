package mcpserver_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
)

// readOnlyTools are the contract tools that never write. A tool added to api/mcp-tools.json must
// be named here or given a case in writeCases, which is what keeps the revoked-credential table
// below complete. dev_status, dev_logs and dev_smoke are not listed here even though they only
// read mainloop-dev: each still inserts a devloop_operations row in Mainloop's own database, so
// they are write cases too.
var readOnlyTools = []string{"get_pull_request_merge_status", "task_get", "task_history", "task_list", "topics", "whoami"}

// A writeCase calls one write tool with arguments valid enough to reach its write. prepare runs
// while the credential is live and returns the arguments.
type writeCase struct {
	tool    string
	path    string
	prs     bool
	devloop bool
	role    dbstate.BindingRole
	grant   dbstate.MCPGrantKind
	prepare func(t *testing.T, h *harness, p *prHarness, a authtest.Seeded) map[string]any
}

// newDevloopHarness serves the devloop tools with the flag on, a DB-only lease and an operations
// service with no Kubernetes or GitHub dependency: enough to prove the write each tool makes
// (the lease row, or the devloop_operations row) without needing a real cluster.
func newDevloopHarness(t *testing.T) *harness {
	t.Helper()
	return buildHarness(t, true, true, func(srv *mcpserver.Server, pool *pgxpool.Pool) {
		srv.Policy.DevLoop = true
		srv.DevLease = &devloop.LeaseService{Pool: pool}
		srv.DevOps = &devloop.OperationsService{Pool: pool}
	})
}

// seedDevloopAgent seeds a workspace agent bound to the one project the devloop tools allow.
func seedDevloopAgent(t *testing.T, h *harness) authtest.Seeded {
	t.Helper()
	ctx := context.Background()
	if _, err := h.env.Pool.Exec(ctx, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url, default_branch)
		VALUES ('devloop-p', 'owner', 'oldsj', 'mainloop', 'oldsj/mainloop', 'https://x', 'main') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	a := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	if _, err := h.env.Pool.Exec(ctx, `UPDATE sessions SET project_id = 'devloop-p' WHERE id = $1`, a.SessionID); err != nil {
		t.Fatal(err)
	}
	return a
}

func setupCall(t *testing.T, h *harness, a authtest.Seeded, tool string, args map[string]any) map[string]any {
	t.Helper()
	_, r := h.call(ordinary, a.Token, tool, args)
	if r.Result.IsError {
		t.Fatalf("set-up %s: %q", tool, r.text())
	}
	return r.Result.StructuredContent
}

func mergeArgs() map[string]any {
	return map[string]any{"project_id": "p1", "pr_number": 1, "expected_sha": prTip, "request_id": "dead"}
}

var writeCases = []writeCase{
	{tool: "topic_open", role: dbstate.RoleMain, grant: dbstate.GrantCoordination,
		prepare: func(*testing.T, *harness, *prHarness, authtest.Seeded) map[string]any {
			return map[string]any{"name": "release", "status": "cutting 1.0"}
		}},
	{tool: "note", role: dbstate.RoleMain, grant: dbstate.GrantCoordination,
		prepare: func(*testing.T, *harness, *prHarness, authtest.Seeded) map[string]any {
			return map[string]any{"text": "remember"}
		}},
	{tool: "decide", role: dbstate.RoleMain, grant: dbstate.GrantCoordination,
		prepare: func(*testing.T, *harness, *prHarness, authtest.Seeded) map[string]any {
			return map[string]any{"text": "use Go"}
		}},
	{tool: "pending_add", role: dbstate.RoleMain, grant: dbstate.GrantCoordination,
		prepare: func(*testing.T, *harness, *prHarness, authtest.Seeded) map[string]any {
			return map[string]any{"text": "tag it"}
		}},
	{tool: "pending_done", role: dbstate.RoleMain, grant: dbstate.GrantCoordination,
		prepare: func(t *testing.T, h *harness, _ *prHarness, a authtest.Seeded) map[string]any {
			setupCall(t, h, a, "pending_add", map[string]any{"text": "close me"})
			var id string
			if err := h.env.Pool.QueryRow(context.Background(), `SELECT id FROM topic_records WHERE text = 'close me'`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			return map[string]any{"id": id}
		}},
	{tool: "delegate", role: dbstate.RoleMain, grant: dbstate.GrantCoordination,
		prepare: func(*testing.T, *harness, *prHarness, authtest.Seeded) map[string]any {
			return map[string]any{"request_id": "dead", "title": "Plan", "brief": "Make a plan", "mode": "coordination"}
		}},
	{tool: "task_cancel", role: dbstate.RoleMain, grant: dbstate.GrantCoordination,
		prepare: func(t *testing.T, h *harness, _ *prHarness, a authtest.Seeded) map[string]any {
			made := setupCall(t, h, a, "delegate", map[string]any{"request_id": "live", "title": "Plan", "brief": "b", "mode": "coordination"})
			id, _ := made["task_id"].(string)
			var version int64
			if err := h.env.Pool.QueryRow(context.Background(), `SELECT version FROM tasks WHERE id = $1`, id).Scan(&version); err != nil {
				t.Fatal(err)
			}
			return map[string]any{"request_id": "dead", "task_id": id, "expected_version": version, "expected_attempt_id": nil}
		}},
	{tool: "report", role: dbstate.RoleSupervisor, grant: dbstate.GrantCoordination,
		prepare: func(t *testing.T, h *harness, _ *prHarness, a authtest.Seeded) map[string]any {
			task, attempt := h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptActive, a)
			return map[string]any{"task_id": task, "attempt_id": attempt, "request_id": "dead", "summary": "half way", "outcome": "progress"}
		}},
	{tool: "open_pull_request", prs: true,
		prepare: func(*testing.T, *harness, *prHarness, authtest.Seeded) map[string]any {
			return map[string]any{"project_id": "p1", "branch": "feature", "expected_sha": prTip, "title": "Add a thing", "body": "Because.", "request_id": "dead"}
		}},
	{tool: "prepare_pull_request_merge", prs: true,
		prepare: func(*testing.T, *harness, *prHarness, authtest.Seeded) map[string]any { return mergeArgs() }},
	{tool: "merge_pull_request", prs: true,
		prepare: func(*testing.T, *harness, *prHarness, authtest.Seeded) map[string]any { return mergeArgs() }},
	{tool: "merge_pull_request_with_approval", prs: true, path: mcpserver.PathApproval,
		prepare: func(t *testing.T, _ *harness, p *prHarness, a authtest.Seeded) map[string]any {
			if _, err := p.pool.Exec(context.Background(), `UPDATE projects SET merge_policy = 'approval' WHERE id = 'p1'`); err != nil {
				t.Fatal(err)
			}
			args := mergeArgs()
			args["request_id"] = "live"
			proposal, _ := setupCall(t, p.harness, a, "merge_pull_request", args)["proposal_id"].(string)
			if _, err := p.pool.Exec(context.Background(), `UPDATE agent_bindings SET kagent_session_id = 'k1' WHERE session_id = $1`, a.SessionID); err != nil {
				t.Fatal(err)
			}
			p.consent(t, a.SessionID, proposal, "dead")
			return map[string]any{"proposal_id": proposal, "request_id": "dead"}
		}},
	{tool: "dev_lease_acquire", devloop: true,
		prepare: func(*testing.T, *harness, *prHarness, authtest.Seeded) map[string]any { return map[string]any{} }},
	{tool: "dev_lease_release", devloop: true,
		prepare: func(*testing.T, *harness, *prHarness, authtest.Seeded) map[string]any { return map[string]any{} }},
	{tool: "dev_sync", devloop: true,
		prepare: func(t *testing.T, h *harness, _ *prHarness, a authtest.Seeded) map[string]any {
			setupCall(t, h, a, "dev_lease_acquire", map[string]any{})
			return map[string]any{"commit_sha": prTip}
		}},
	{tool: "dev_status", devloop: true,
		prepare: func(*testing.T, *harness, *prHarness, authtest.Seeded) map[string]any { return map[string]any{} }},
}

// dump is a digest of every table: its row count and a hash of all its rows, so a write to any
// column of any table shows.
func dump(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = current_schema() ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, name := range tables {
		var n int
		var sum string
		q := fmt.Sprintf(`SELECT count(*), coalesce(md5(string_agg(x::text, '|' ORDER BY x::text)), '') FROM %q x`, name)
		if err := pool.QueryRow(ctx, q).Scan(&n, &sum); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&out, "%s %d %s\n", name, n, sum)
	}
	return out.String()
}

func TestEveryWriteToolIsRefusedAfterItsCredentialIsRevoked(t *testing.T) {
	c := loadContract(t)
	var covered []string
	for _, w := range writeCases {
		covered = append(covered, w.tool)
	}
	var contractNames []string
	for _, tool := range c.Tools {
		contractNames = append(contractNames, tool.Name)
		if !slices.Contains(covered, tool.Name) && !slices.Contains(readOnlyTools, tool.Name) {
			t.Errorf("%s is in api/mcp-tools.json but is neither a write case nor read-only", tool.Name)
		}
		if slices.Contains(covered, tool.Name) && slices.Contains(readOnlyTools, tool.Name) {
			t.Errorf("%s is both a write case and read-only", tool.Name)
		}
	}
	for _, name := range append(slices.Clone(covered), readOnlyTools...) {
		if !slices.Contains(contractNames, name) {
			t.Errorf("%s is classified here but is not in api/mcp-tools.json", name)
		}
	}

	for _, w := range writeCases {
		t.Run(w.tool, func(t *testing.T) {
			var h *harness
			var p *prHarness
			var a authtest.Seeded
			switch {
			case w.prs:
				p = newPRHarness(t)
				h = p.harness
				a = p.seedAgent(t, nil)
			case w.devloop:
				h = newDevloopHarness(t)
				a = seedDevloopAgent(t, h)
			default:
				h = newTaskHarness(t, true)
				a = h.env.Seed(t, authtest.Agent{Role: w.role, Grant: w.grant})
			}
			path := w.path
			if path == "" {
				path = ordinary
			}
			args := w.prepare(t, h, p, a)

			// The credential is valid when the request authenticates and revoked before the tool
			// runs, which is the window the guard closes. The digest is taken after the revoke,
			// which is itself a write.
			var before string
			orig := h.auth.Depth
			h.auth.Depth = func(ctx context.Context, row dbq.GetMCPActorRow) (int, bool, error) {
				if row.BindingID == a.SessionID {
					if _, err := agentauth.RevokeInTx(ctx, dbq.New(h.env.Pool), row.BindingID); err != nil {
						return 0, false, err
					}
					before = dump(t, h.env.Pool)
				}
				return orig(ctx, row)
			}
			w2, r := h.call(path, a.Token, w.tool, args)
			if w2.Code != 200 || !r.Result.IsError || r.text() != "[unauthorized] the credential is no longer valid" {
				t.Fatalf("%s with a revoked credential = %d, error %v, %q", w.tool, w2.Code, r.Result.IsError, r.text())
			}
			if before == "" {
				t.Fatal("the credential was never revoked")
			}
			if after := dump(t, h.env.Pool); after != before {
				t.Errorf("%s wrote after the revoke:\nbefore:\n%s\nafter:\n%s", w.tool, before, after)
			}
		})
	}
}

func TestTheDigestSeesEveryTable(t *testing.T) {
	h := newHarness(t, false)
	if d := dump(t, h.env.Pool); !strings.Contains(d, "topic_records ") || !strings.Contains(d, "pr_creations ") || !strings.Contains(d, "credentials ") {
		t.Fatalf("digest misses tables:\n%s", d)
	}
}
