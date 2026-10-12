package mcpserver_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
	"github.com/oldsj/mainloop/server/internal/merge"
	"github.com/oldsj/mainloop/server/internal/prs"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

const (
	prTip   = "1111111111111111111111111111111111111111"
	mergeIt = "/repos/o/n/pulls/1/merge"
)

type prHarness struct {
	*harness
	fake *githubtest.Server
	pool *pgxpool.Pool
}

// newPRHarness serves the pull-request tools over a fake GitHub with one repository, and runs the
// creation and merge loops that the API process would run.
func newPRHarness(t *testing.T) *prHarness {
	t.Helper()
	fake := githubtest.New(t)
	fake.AddRepo(githubtest.Repository{
		Owner: "o", Name: "n", ID: 7, Branches: map[string]string{"feature": prTip},
		PullRequests: []githubtest.PullRequest{{
			Number: 1, Title: "Add a thing", HeadRef: "feature", HeadSHA: prTip,
			Files: []githubtest.File{{Filename: "server/a.go", Additions: 1}},
		}},
		Suites: map[string][]githubtest.CheckSuite{prTip: {{ID: 1, AppID: 9, Conclusion: "success"}}},
		Runs:   map[string][]githubtest.CheckRun{prTip: {{ID: 1, AppID: 9, Name: "ci", Conclusion: "success"}}},
	})
	app, err := github.NewApp(github.Options{AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfgs := []config.MergeConfiguration{{
		TemplateName: "t", Provider: config.NativeClaude, CompiledAlias: "mainloop_merge",
		Tool: config.MergeToolName, RequireApproval: true, Operation: config.MergeOperation,
	}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &prHarness{fake: fake}
	h.harness = buildHarness(t, true, false, func(srv *mcpserver.Server, pool *pgxpool.Pool) {
		h.pool = pool
		srv.PRs = &prs.Service{Pool: pool, GitHub: app, Log: log, Settle: 5 * time.Second, Recheck: 10 * time.Millisecond, Ceiling: 20 * time.Millisecond}
		srv.Merge = &merge.Service{
			Pool: pool, GitHub: app, Log: log, Configs: cfgs, Runtime: staticRuntime{}, Settle: 5 * time.Second, Poll: 20 * time.Millisecond,
			Recheck: 10 * time.Millisecond, Ceiling: 20 * time.Millisecond,
		}
		r := reconcile.NewRunner(pool, log, nil)
		pl, ml := srv.PRs.Loop(), srv.Merge.Loop()
		pl.Interval, ml.Interval = 10*time.Millisecond, 10*time.Millisecond
		r.Add(pl)
		r.Add(ml)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = r.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
	})
	return h
}

// staticRuntime is the kagent read of the one Session the tests record consent for: k1 runs on
// the Agent ns/a, which was created from the template "t".
type staticRuntime struct{}

func (staticRuntime) SessionAgent(_ context.Context, id string) (kagent.AgentRef, error) {
	if id != "k1" {
		return kagent.AgentRef{}, fmt.Errorf("unknown session %q", id)
	}
	return kagent.AgentRef{Namespace: "ns", Name: "a"}, nil
}

func (staticRuntime) AgentTemplate(context.Context, kagent.AgentRef) (string, error) { return "t", nil }

// seedProject makes the project and puts an agent in it.
func (h *prHarness) seedAgent(t *testing.T, mut func(*authtest.Agent)) authtest.Seeded {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(), `INSERT INTO projects (id, user_id, owner, name, full_name, html_url, default_branch)
		VALUES ('p1', 'owner', 'o', 'n', 'o/n', 'https://x', 'main') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	a := authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace}
	if mut != nil {
		mut(&a)
	}
	s := h.env.Seed(t, a)
	if _, err := h.pool.Exec(context.Background(), `UPDATE sessions SET project_id = 'p1', branch_name = 'feature' WHERE id = $1`, s.SessionID); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBothSurfacesListTheMergeToolsByRoute(t *testing.T) {
	h := newPRHarness(t)
	a := h.seedAgent(t, nil)
	want := []string{"get_pull_request_merge_status", "merge_pull_request", "open_pull_request", "prepare_pull_request_merge", "whoami"}
	if got := h.list(t, mcpserver.PathOrdinary, a.Token); !slices.Equal(got, want) {
		t.Errorf("/mcp lists %v, want %v", got, want)
	}
	// The approval surface serves the one tool that executes an approved merge, alone.
	want = []string{"merge_pull_request_with_approval"}
	if got := h.list(t, mcpserver.PathApproval, a.Token); !slices.Equal(got, want) {
		t.Errorf("/mcp/merge-approval lists %v, want %v", got, want)
	}
}

func TestOpenPullRequestOnceAndReplay(t *testing.T) {
	h := newPRHarness(t)
	a := h.seedAgent(t, nil)
	args := map[string]any{
		"project_id": "p1", "branch": "feature", "expected_sha": prTip, "title": "Add a thing", "body": "Because.", "request_id": "o1",
	}
	// The branch already has pull request #1, so the open adopts it rather than POSTing a second.
	w, first := h.call(mcpserver.PathOrdinary, a.Token, "open_pull_request", args)
	sc := first.Result.StructuredContent
	if w.Code != http.StatusOK || first.Result.IsError || sc["state"] != "created" || sc["pr_number"] != float64(1) {
		t.Fatalf("open = %d %s", w.Code, w.Body)
	}
	_, again := h.call(mcpserver.PathOrdinary, a.Token, "open_pull_request", args)
	if again.Result.IsError || again.Result.StructuredContent["id"] != sc["id"] {
		t.Errorf("replay = %v", again.Result.StructuredContent)
	}
	if n := h.fake.Count("POST", "/repos/o/n/pulls"); n != 0 {
		t.Errorf("POSTs = %d, want 0", n)
	}
	changed := map[string]any{
		"project_id": "p1", "branch": "feature", "expected_sha": prTip, "title": "Other", "body": "", "request_id": "o1",
	}
	if _, r := h.call(mcpserver.PathOrdinary, a.Token, "open_pull_request", changed); !r.Result.IsError || r.text() != "[request_payload_conflict] this request_id was used with other arguments" {
		t.Errorf("changed arguments = %v %q", r.Result.IsError, r.text())
	}
}

func TestProjectAuthorityFailsClosed(t *testing.T) {
	h := newPRHarness(t)
	a := h.seedAgent(t, nil)
	bare := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	open := map[string]any{"project_id": "p1", "branch": "feature", "expected_sha": prTip, "title": "t", "body": "", "request_id": "x"}
	merge := map[string]any{"project_id": "p1", "pr_number": 1, "expected_sha": prTip, "request_id": "x"}
	for _, tok := range []string{bare.Token} {
		if _, r := h.call(mcpserver.PathOrdinary, tok, "open_pull_request", open); !r.Result.IsError || r.text()[:11] != "[forbidden]" {
			t.Errorf("open without a project = %q", r.text())
		}
		if _, r := h.call(mcpserver.PathOrdinary, tok, "merge_pull_request", merge); !r.Result.IsError || r.text()[:11] != "[forbidden]" {
			t.Errorf("merge without a project = %q", r.text())
		}
	}
	open["project_id"] = "other"
	if _, r := h.call(mcpserver.PathOrdinary, a.Token, "open_pull_request", open); !r.Result.IsError || r.text()[:11] != "[forbidden]" {
		t.Errorf("open in another project = %q", r.text())
	}
	// Bad arguments are validation errors before anything is read.
	merge["expected_sha"] = "nope"
	if _, r := h.call(mcpserver.PathOrdinary, a.Token, "merge_pull_request", merge); !r.Result.IsError || r.text() != "[validation] invalid tool arguments" {
		t.Errorf("bad sha = %q", r.text())
	}
}

func TestMergeToolsPrepareMergeAndStatus(t *testing.T) {
	h := newPRHarness(t)
	a := h.seedAgent(t, nil)
	args := map[string]any{"project_id": "p1", "pr_number": 1, "expected_sha": prTip, "request_id": "m1"}

	_, prep := h.call(mcpserver.PathOrdinary, a.Token, "prepare_pull_request_merge", args)
	sc := prep.Result.StructuredContent
	if prep.Result.IsError || sc["status"] != "prepared" || sc["route"] != "auto" || sc["proposal_id"] == nil {
		t.Fatalf("prepare = %v %q", sc, prep.text())
	}
	proposal := sc["proposal_id"].(string)

	// An auto-policy PR with green CI merges on the call, once.
	args["request_id"] = "m2"
	_, done := h.call(mcpserver.PathOrdinary, a.Token, "merge_pull_request", args)
	if done.Result.IsError || done.Result.StructuredContent["status"] != "merged" {
		t.Fatalf("merge = %v %q", done.Result.StructuredContent, done.text())
	}
	_, again := h.call(mcpserver.PathOrdinary, a.Token, "merge_pull_request", args)
	if again.Result.IsError || again.Result.StructuredContent["status"] != "merged" || h.fake.Count("PUT", mergeIt) != 1 {
		t.Errorf("replay = %v, PUTs %d", again.Result.StructuredContent, h.fake.Count("PUT", mergeIt))
	}

	// The status is a read: the first proposal was superseded by the second preparation.
	_, st := h.call(mcpserver.PathOrdinary, a.Token, "get_pull_request_merge_status", map[string]any{"proposal_id": proposal, "request_id": "m1"})
	if st.Result.IsError || st.Result.StructuredContent["status"] != "superseded" {
		t.Errorf("status = %v %q", st.Result.StructuredContent, st.text())
	}
	if _, r := h.call(mcpserver.PathOrdinary, a.Token, "get_pull_request_merge_status", map[string]any{"proposal_id": proposal, "request_id": "unknown"}); !r.Result.IsError {
		t.Error("a request id that does not belong to the proposal was answered")
	}
}

func TestApprovalToolRefusesWithoutConsentAndIsOnlyOnTheApprovalSurface(t *testing.T) {
	h := newPRHarness(t)
	a := h.seedAgent(t, nil)
	if _, err := h.pool.Exec(context.Background(), `UPDATE projects SET merge_policy = 'approval' WHERE id = 'p1'`); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"project_id": "p1", "pr_number": 1, "expected_sha": prTip, "request_id": "m1"}
	_, v := h.call(mcpserver.PathOrdinary, a.Token, "merge_pull_request", args)
	sc := v.Result.StructuredContent
	if v.Result.IsError || sc["status"] != "approval_required" || h.fake.Count("PUT", mergeIt) != 0 {
		t.Fatalf("merge = %v %q", sc, v.text())
	}
	call := map[string]any{"proposal_id": sc["proposal_id"], "request_id": "a1"}
	_, r := h.call(mcpserver.PathApproval, a.Token, "merge_pull_request_with_approval", call)
	if !r.Result.IsError || h.fake.Count("PUT", mergeIt) != 0 {
		t.Errorf("approved merge without consent = %v %q, PUTs %d", r.Result.IsError, r.text(), h.fake.Count("PUT", mergeIt))
	}
	// The ordinary surface has no such tool.
	_, bad := h.call(mcpserver.PathOrdinary, a.Token, "merge_pull_request_with_approval", call)
	if bad.Error == nil && !bad.Result.IsError {
		t.Error("the ordinary surface ran the approval tool")
	}
}

// consent records the owner's approval of one exact approved-merge call, as hitl does.
func (h *prHarness) consent(t *testing.T, session, proposal, request string) {
	t.Helper()
	name := "mcp__mainloop_merge__" + config.MergeToolName
	sum := sha256.Sum256([]byte(fmt.Sprintf(`{"proposal_id":%q,"request_id":%q}`, proposal, request)))
	req, _ := json.Marshal(map[string]any{"type": "tool_approval", "hint": "h", "nested": nil, "tools": []map[string]any{{
		"id": "t1", "call_id": "c1", "name": name, "args": map[string]any{"proposal_id": proposal, "request_id": request},
	}}})
	receipt, _ := json.Marshal(map[string]any{"calls": []map[string]any{{
		"approved": true, "call_id": "c1", "tool_name": name, "arguments_hash": hex.EncodeToString(sum[:]),
	}}})
	id := "req-" + request
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO hitl_requests (id, owner_id, session_id, kagent_session_id, agent_namespace, agent_name, context_id, task_id, status_message_id, request_hash, kind, payload)
		  VALUES ($1, 'owner', $2, 'k1', 'ns', 'a', 'ctx', $1, 'm', 'h', 'tool_approval', $3)`, []any{id, session, req}},
		{`INSERT INTO hitl_responses (id, request_id, owner_id, idempotency_key, decision, decision_hash, outbound_message_id, receipt, state)
		  VALUES ($1, $1, 'owner', $1, '{}', 'h', $1, $2, 'accepted')`, []any{id, receipt}},
	} {
		if _, err := h.pool.Exec(context.Background(), q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestApprovedMergeOnTheApprovalSurface(t *testing.T) {
	h := newPRHarness(t)
	a := h.seedAgent(t, nil)
	if _, err := h.pool.Exec(context.Background(), `UPDATE projects SET merge_policy = 'approval' WHERE id = 'p1'`); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"project_id": "p1", "pr_number": 1, "expected_sha": prTip, "request_id": "m1"}
	_, v := h.call(mcpserver.PathOrdinary, a.Token, "merge_pull_request", args)
	proposal, _ := v.Result.StructuredContent["proposal_id"].(string)
	if proposal == "" {
		t.Fatalf("no proposal: %q", v.text())
	}
	// The agent's binding needs a provider session for the consent to be attributed to it.
	if _, err := h.pool.Exec(context.Background(), `UPDATE agent_bindings SET kagent_session_id = 'k1' WHERE session_id = $1`, a.SessionID); err != nil {
		t.Fatal(err)
	}
	h.consent(t, a.SessionID, proposal, "a1")
	call := map[string]any{"proposal_id": proposal, "request_id": "a1"}
	_, done := h.call(mcpserver.PathApproval, a.Token, "merge_pull_request_with_approval", call)
	if done.Result.IsError || done.Result.StructuredContent["status"] != "merged" {
		t.Fatalf("approved merge = %v %q", done.Result.StructuredContent, done.text())
	}
	_, again := h.call(mcpserver.PathApproval, a.Token, "merge_pull_request_with_approval", call)
	if again.Result.IsError || h.fake.Count("PUT", mergeIt) != 1 {
		t.Errorf("replay = %q, PUTs %d", again.text(), h.fake.Count("PUT", mergeIt))
	}
	_, st := h.call(mcpserver.PathOrdinary, a.Token, "get_pull_request_merge_status", map[string]any{"proposal_id": proposal, "request_id": "a1"})
	if st.Result.IsError || st.Result.StructuredContent["status"] != "merged" {
		t.Errorf("status = %v %q", st.Result.StructuredContent, st.text())
	}
}

// otherProjectAgent seeds an agent of the same owner that is bound to a different project, or to none.
func (h *prHarness) otherProjectAgent(t *testing.T, project string) authtest.Seeded {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(), `INSERT INTO projects (id, user_id, owner, name, full_name, html_url, default_branch)
		VALUES ('p2', 'owner', 'o', 'other', 'o/other', 'https://x', 'main') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	s := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	if project != "" {
		if _, err := h.pool.Exec(context.Background(), `UPDATE sessions SET project_id = $2 WHERE id = $1`, s.SessionID, project); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestReviewStatusMustBeProjectScoped(t *testing.T) {
	h := newPRHarness(t)
	a := h.seedAgent(t, nil)
	_, prep := h.call(mcpserver.PathOrdinary, a.Token, "prepare_pull_request_merge",
		map[string]any{"project_id": "p1", "pr_number": 1, "expected_sha": prTip, "request_id": "m1"})
	proposal, _ := prep.Result.StructuredContent["proposal_id"].(string)
	if proposal == "" {
		t.Fatalf("no proposal: %q", prep.text())
	}
	status := map[string]any{"proposal_id": proposal, "request_id": "m1"}
	for name, project := range map[string]string{"another project": "p2", "no project": ""} {
		o := h.otherProjectAgent(t, project)
		if _, r := h.call(mcpserver.PathOrdinary, o.Token, "get_pull_request_merge_status", status); !r.Result.IsError || r.text()[:11] != "[forbidden]" {
			t.Errorf("status from %s = %v %q", name, r.Result.StructuredContent, r.text())
		}
	}
	if _, r := h.call(mcpserver.PathOrdinary, a.Token, "get_pull_request_merge_status", status); r.Result.IsError {
		t.Errorf("status from the proposal's project = %q", r.text())
	}
}

func TestReviewApprovalMustBeProjectScoped(t *testing.T) {
	h := newPRHarness(t)
	a := h.seedAgent(t, nil)
	if _, err := h.pool.Exec(context.Background(), `UPDATE projects SET merge_policy = 'approval' WHERE id = 'p1'`); err != nil {
		t.Fatal(err)
	}
	_, v := h.call(mcpserver.PathOrdinary, a.Token, "merge_pull_request",
		map[string]any{"project_id": "p1", "pr_number": 1, "expected_sha": prTip, "request_id": "m1"})
	proposal, _ := v.Result.StructuredContent["proposal_id"].(string)
	if _, err := h.pool.Exec(context.Background(), `UPDATE agent_bindings SET kagent_session_id = 'k1' WHERE session_id = $1`, a.SessionID); err != nil {
		t.Fatal(err)
	}
	h.consent(t, a.SessionID, proposal, "a1")
	call := map[string]any{"proposal_id": proposal, "request_id": "a1"}
	for name, project := range map[string]string{"another project": "p2", "no project": ""} {
		// The same session binding, now with the wrong project: the proposal's session and receipt are
		// right, and only the project differs.
		o := h.otherProjectAgent(t, project)
		if _, r := h.call(mcpserver.PathApproval, o.Token, "merge_pull_request_with_approval", call); !r.Result.IsError || r.text()[:11] != "[forbidden]" {
			t.Errorf("approval from %s = %v %q", name, r.Result.StructuredContent, r.text())
		}
	}
	if n := h.fake.Count("PUT", mergeIt); n != 0 {
		t.Fatalf("PUTs = %d", n)
	}
	if _, err := h.pool.Exec(context.Background(), `UPDATE sessions SET project_id = 'p2' WHERE id = $1`, a.SessionID); err != nil {
		t.Fatal(err)
	}
	// The proposal's own session moved to another project loses authority as well.
	if _, r := h.call(mcpserver.PathApproval, a.Token, "merge_pull_request_with_approval", call); !r.Result.IsError || r.text()[:11] != "[forbidden]" {
		t.Errorf("approval after the session left the project = %q", r.text())
	}
	if n := h.fake.Count("PUT", mergeIt); n != 0 {
		t.Fatalf("PUTs = %d", n)
	}
}
