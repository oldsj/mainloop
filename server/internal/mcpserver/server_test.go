package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/tasks"
	"github.com/oldsj/mainloop/server/internal/testdb"
	"github.com/oldsj/mainloop/server/internal/topics"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const host = "mainloop-mcp.test"

type harness struct {
	env     *authtest.Env
	handler http.Handler
}

func newHarness(t *testing.T, mergeTools bool) *harness {
	return buildHarness(t, mergeTools, false, nil)
}

// newTaskHarness authenticates supervisors and children through their task attempts, as serveMCP
// does. The plain harness gives the task roles a fixed depth instead.
func newTaskHarness(t *testing.T, mergeTools bool) *harness {
	return buildHarness(t, mergeTools, true, nil)
}

func buildHarness(t *testing.T, mergeTools, realDepth bool, mod func(*mcpserver.Server, *pgxpool.Pool)) *harness {
	t.Helper()
	d := testdb.New(t)
	env := authtest.NewEnv(d.Pool)
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		t.Fatal(err)
	}
	taskSvc := &tasks.Service{Pool: d.Pool, Providers: reg, DefaultProfile: "claude", MergeTools: mergeTools}
	q := dbq.New(d.Pool)
	auth := &agentauth.Authenticator{Q: q, Depth: func(_ context.Context, row dbq.GetMCPActorRow) (int, bool, error) {
		switch row.Role {
		case dbstate.RoleSupervisor:
			return 1, false, nil
		case dbstate.RoleChild:
			return 2, false, nil
		}
		return 0, false, nil
	},
	}
	if realDepth {
		auth.Depth = taskSvc.AuthDepth
	}
	srv := &mcpserver.Server{
		Auth:         auth,
		Policy:       agentauth.Policy{MergeTools: mergeTools},
		Queries:      q,
		Topics:       &topics.Service{Pool: d.Pool},
		Tasks:        taskSvc,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		AllowedHosts: map[string]bool{host: true},
	}
	if mod != nil {
		mod(srv, d.Pool)
	}
	return &harness{env: env, handler: srv.Handler()}
}

// rpc is the parts of a JSON-RPC reply the tests read.
type rpc struct {
	Result struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
		IsError           bool             `json:"isError"`
		Content           []map[string]any `json:"content"`
		StructuredContent map[string]any   `json:"structuredContent"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (r rpc) text() string {
	if len(r.Result.Content) == 0 {
		return ""
	}
	s, _ := r.Result.Content[0]["text"].(string)
	return s
}

func (h *harness) post(path, hostHeader, token string, body any) (*httptest.ResponseRecorder, rpc) {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(raw))
	req.Host = hostHeader
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	var out rpc
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func (h *harness) list(t *testing.T, path, token string) []string {
	t.Helper()
	w, r := h.post(path, host, token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if w.Code != http.StatusOK || r.Error != nil {
		t.Fatalf("tools/list = %d %s", w.Code, w.Body)
	}
	names := []string{}
	for _, tool := range r.Result.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func (h *harness) call(path, token, tool string, args any) (*httptest.ResponseRecorder, rpc) {
	return h.post(path, host, token, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
}

func TestRequiresABearer(t *testing.T) {
	h := newHarness(t, false)
	good := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	for name, token := range map[string]string{
		"none":      "",
		"unknown":   "ml_" + strings.Repeat("0", 64),
		"foreign":   "sk-not-ours",
		"wrong key": agentauth.Token([]byte("another key"), good.CredentialID),
	} {
		for _, path := range []string{mcpserver.PathOrdinary, mcpserver.PathApproval} {
			w, _ := h.post(path, host, token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s: status = %d", name, path, w.Code)
			}
			if w.Header().Get("WWW-Authenticate") == "" {
				t.Errorf("%s %s: no WWW-Authenticate", name, path)
			}
			if strings.Contains(w.Body.String(), token) && token != "" {
				t.Errorf("%s: the response echoes the token", name)
			}
		}
	}
}

func TestHostAllowlist(t *testing.T) {
	h := newHarness(t, false)
	s := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	list := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}
	for _, bad := range []string{"evil.example", "localhost:8002", "127.0.0.1", "mainloop-mcp.test.evil.example"} {
		if w, _ := h.post(mcpserver.PathOrdinary, bad, s.Token, list); w.Code != http.StatusNotFound {
			t.Errorf("Host %q: status = %d, want 404", bad, w.Code)
		}
	}
	if w, _ := h.post(mcpserver.PathOrdinary, host+":8002", s.Token, list); w.Code != http.StatusOK {
		t.Errorf("allowed Host with a port: status = %d", w.Code)
	}
	// Probes address the pod by IP and need no allowed Host.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	req.Host = "10.1.2.3:8002"
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "healthy") {
		t.Errorf("/health = %d %s", w.Code, w.Body)
	}
}

func TestARequestWithAnOriginIsRefused(t *testing.T) {
	h := newHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	for _, path := range []string{mcpserver.PathOrdinary, mcpserver.PathApproval} {
		for _, token := range []string{main.Token, ""} {
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(raw))
			req.Host = host
			req.Header.Set("Origin", "https://example.com")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			h.handler.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s with token=%t: status = %d, want 403", path, token != "", w.Code)
			}
		}
	}
	// The same request without Origin still works.
	if got := h.list(t, mcpserver.PathOrdinary, main.Token); len(got) == 0 {
		t.Error("no tools without an Origin")
	}
}

func TestOnlyPostIsServed(t *testing.T) {
	h := newHarness(t, false)
	s := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/mcp", nil)
	req.Host = host
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp = %d, want 405 (stateless: no standalone stream)", w.Code)
	}
}

func TestDiscoveryFollowsThePerActorTable(t *testing.T) {
	h := newHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	ws := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})

	// With the merge tools off, only open_pull_request is listed of the pull request tools.
	want := []string{
		"decide", "delegate", "note", "open_pull_request", "pending_add", "pending_done", "task_cancel", "task_get",
		"task_history", "task_list", "topic_open", "topics", "whoami",
	}
	if got := h.list(t, mcpserver.PathOrdinary, main.Token); !slices.Equal(got, want) {
		t.Errorf("main lists %v, want %v", got, want)
	}
	if got := h.list(t, mcpserver.PathOrdinary, ws.Token); !slices.Equal(got, []string{"open_pull_request", "whoami"}) {
		t.Errorf("workspace agent lists %v", got)
	}
	if got := h.list(t, mcpserver.PathOrdinary, sup.Token); !slices.Equal(got, []string{"delegate", "report", "task_cancel", "task_get", "task_history", "task_list", "whoami"}) {
		t.Errorf("supervisor lists %v", got)
	}

	// The schemas ship with the listing and forbid extra properties.
	_, r := h.post(mcpserver.PathOrdinary, host, main.Token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	for _, tool := range r.Result.Tools {
		if tool.InputSchema["additionalProperties"] != false {
			t.Errorf("%s allows extra properties", tool.Name)
		}
	}

	// Nothing is on the approval surface unless merge tools are enabled.
	for _, s := range []authtest.Seeded{main, ws, sup} {
		if w, _ := h.post(mcpserver.PathApproval, host, s.Token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}); w.Code != http.StatusForbidden {
			t.Errorf("approval surface = %d, want 403", w.Code)
		}
	}
}

func TestMergeToolsOnlyWhenEnabled(t *testing.T) {
	on := newHarness(t, true)
	ws := on.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	sup := on.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination})
	// With merge tools on, a workspace agent has a tool on the approval surface (not implemented
	// yet, so the listing is empty) while an actor without open_pull_request is still refused.
	list := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}
	if w, _ := on.post(mcpserver.PathApproval, host, ws.Token, list); w.Code != http.StatusOK {
		t.Errorf("workspace agent, approval surface = %d", w.Code)
	}
	if w, _ := on.post(mcpserver.PathApproval, host, sup.Token, list); w.Code != http.StatusForbidden {
		t.Errorf("supervisor, approval surface = %d", w.Code)
	}
}

func TestInvokingATool(t *testing.T) {
	h := newHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	p := mcpserver.PathOrdinary

	w, r := h.call(p, main.Token, "whoami", map[string]any{})
	if w.Code != http.StatusOK || r.Result.IsError {
		t.Fatalf("whoami = %d %s", w.Code, w.Body)
	}
	if want := "main claude session=" + main.SessionID[:8] + " depth=0 grant=active scope=not_enrolled"; r.text() != want {
		t.Errorf("whoami text = %q, want %q", r.text(), want)
	}
	sc := r.Result.StructuredContent
	if sc["session_id"] != main.SessionID || sc["role"] != "main" || sc["mcp_grant_kind"] != "coordination" || sc["text"] != r.text() {
		t.Errorf("structured = %v", sc)
	}

	if _, r = h.call(p, main.Token, "topics", nil); r.text() != "(no topics yet)" {
		t.Errorf("topics = %q", r.text())
	}
	if _, r = h.call(p, main.Token, "topic_open", map[string]any{"name": " release ", "status": "cutting 1.0"}); r.text() != "topic release ready" {
		t.Errorf("topic_open = %q", r.text())
	}
	if _, r = h.call(p, main.Token, "pending_add", map[string]any{"text": "tag it", "topic": "release"}); !strings.HasPrefix(r.text(), "pending recorded in release (") {
		t.Errorf("pending_add = %q", r.text())
	}
	_, r = h.call(p, main.Token, "note", map[string]any{"text": "remember"})
	if !strings.HasPrefix(r.text(), "note recorded in inbox (") {
		t.Errorf("note = %q", r.text())
	}
	if _, r = h.call(p, main.Token, "decide", map[string]any{"text": "use Go", "topic": "release"}); !strings.HasPrefix(r.text(), "decision recorded in release (") {
		t.Errorf("decide = %q", r.text())
	}
	if _, r = h.call(p, main.Token, "topics", nil); r.text() != "- inbox: (no status) [0 pending]\n- release: cutting 1.0 [1 pending]" && r.text() != "- release: cutting 1.0 [1 pending]\n- inbox: (no status) [0 pending]" {
		t.Errorf("topics = %q", r.text())
	}

	pendingID := h.pendingID(t, main.UserID)
	if _, r = h.call(p, main.Token, "pending_done", map[string]any{"id": pendingID[:8]}); r.text() != "pending closed" || r.Result.IsError {
		t.Errorf("pending_done = %q", r.text())
	}
	if _, r = h.call(p, main.Token, "pending_done", map[string]any{"id": pendingID}); !r.Result.IsError || r.text() != "[404] no such open pending item" {
		t.Errorf("closing again = %v %q", r.Result.IsError, r.text())
	}
}

func (h *harness) pendingID(t *testing.T, user string) string {
	t.Helper()
	var id string
	err := h.env.Pool.QueryRow(context.Background(), `
		SELECT r.id FROM topic_records r JOIN topics t ON t.id = r.topic_id
		WHERE t.user_id = $1 AND r.kind = 'pending' AND r.status = 'open'`, user).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestInvalidArgumentsAreToolErrors(t *testing.T) {
	h := newHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	long := strings.Repeat("x", 2001)
	for name, c := range map[string]struct {
		tool string
		args any
	}{
		"unknown field":        {"note", map[string]any{"text": "a", "extra": 1}},
		"missing text":         {"note", map[string]any{}},
		"blank text":           {"note", map[string]any{"text": "   "}},
		"text too long":        {"decide", map[string]any{"text": long}},
		"topic too long":       {"pending_add", map[string]any{"text": "a", "topic": strings.Repeat("t", 81)}},
		"blank topic name":     {"topic_open", map[string]any{"name": " "}},
		"status too long":      {"topic_open", map[string]any{"name": "a", "status": long}},
		"short pending id":     {"pending_done", map[string]any{"id": "abc"}},
		"wrong type":           {"topic_open", map[string]any{"name": 5}},
		"arguments on whoami":  {"whoami", map[string]any{"x": 1}},
		"arguments not object": {"topics", []int{1}},
	} {
		w, r := h.call(mcpserver.PathOrdinary, main.Token, c.tool, c.args)
		if w.Code != http.StatusOK || !r.Result.IsError || r.text() != "[validation] invalid tool arguments" {
			t.Errorf("%s: %d isError=%v text=%q err=%v", name, w.Code, r.Result.IsError, r.text(), r.Error)
		}
	}
}

func TestAToolOutsideTheActorsTableCannotBeCalled(t *testing.T) {
	h := newHarness(t, false)
	ws := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	for _, tool := range []string{"topics", "note", "topic_open", "pending_done", "delegate", "merge_pull_request"} {
		w, r := h.call(mcpserver.PathOrdinary, ws.Token, tool, map[string]any{"text": "x", "name": "x", "id": "12345678"})
		if r.Error == nil && !r.Result.IsError {
			t.Errorf("%s was served to a workspace agent: %d %s", tool, w.Code, w.Body)
		}
	}
	var topicCount int
	if err := h.env.Pool.QueryRow(context.Background(), `SELECT count(*) FROM topics`).Scan(&topicCount); err != nil || topicCount != 0 {
		t.Errorf("topics = %d, %v: a refused call wrote", topicCount, err)
	}
}

func TestStaleBindingsAreRefusedOnTheNextCall(t *testing.T) {
	h := newHarness(t, false)
	s := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	list := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}
	if w, _ := h.post(mcpserver.PathOrdinary, host, s.Token, list); w.Code != http.StatusOK {
		t.Fatalf("live agent = %d", w.Code)
	}
	if _, err := h.env.Pool.Exec(context.Background(), `UPDATE sessions SET status='completed' WHERE id=$1`, s.SessionID); err != nil {
		t.Fatal(err)
	}
	if w, _ := h.post(mcpserver.PathOrdinary, host, s.Token, list); w.Code != http.StatusUnauthorized {
		t.Errorf("completed session = %d, want 401", w.Code)
	}

	r := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	if _, err := h.env.Creds.Revoke(context.Background(), r.SessionID); err != nil {
		t.Fatal(err)
	}
	if w, _ := h.post(mcpserver.PathOrdinary, host, r.Token, list); w.Code != http.StatusUnauthorized {
		t.Errorf("revoked credential = %d, want 401", w.Code)
	}
}

func TestWhoamiOfAWorkspaceAgentReportsItsScope(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	q := dbq.New(h.env.Pool)
	if _, err := q.CreateProject(ctx, dbq.CreateProjectParams{
		ID: "p1", UserID: "owner", Owner: "oldsj", Name: "mainloop", FullName: "oldsj/mainloop",
		DefaultBranch: "main", HTMLURL: "https://example.invalid/oldsj/mainloop",
	}); err != nil {
		t.Fatal(err)
	}
	s := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	_, r := h.call(mcpserver.PathOrdinary, s.Token, "whoami", nil)
	if sc := r.Result.StructuredContent; sc["scope_status"] != "scope_unavailable" || sc["repository"] != nil {
		t.Errorf("unscoped workspace agent: %v", sc)
	}
	if _, err := h.env.Pool.Exec(ctx, `UPDATE sessions SET project_id='p1', branch_name='feature-x' WHERE id=$1`, s.SessionID); err != nil {
		t.Fatal(err)
	}
	_, r = h.call(mcpserver.PathOrdinary, s.Token, "whoami", nil)
	sc := r.Result.StructuredContent
	if sc["scope_status"] != "available" || sc["grant_status"] != "active" || sc["repository"] != "oldsj/mainloop" ||
		sc["branch"] != "feature-x" || sc["project_id"] != "p1" || sc["workspace_id"] != s.SessionID {
		t.Errorf("scoped workspace agent: %v", sc)
	}
}
