package mcpserver_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
	"github.com/oldsj/mainloop/server/internal/webfetch"
)

// fakeFetcher stands in for the guarded fetcher (internal/webfetch has its own tests against a
// local server): it answers every URL with page, or err, and records what it was asked.
type fakeFetcher struct {
	mu   sync.Mutex
	urls []string
	page *webfetch.Page
	err  error
}

func (f *fakeFetcher) Fetch(_ context.Context, u string) (*webfetch.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls = append(f.urls, u)
	if f.page == nil && f.err == nil {
		return &webfetch.Page{URL: u, FinalURL: u, Status: 200, ContentType: "text/plain", Text: "hello", Bytes: 5}, nil
	}
	return f.page, f.err
}

func (f *fakeFetcher) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.urls)
}

func newWebFetchHarness(t *testing.T, f *fakeFetcher, perMinute int) *harness {
	t.Helper()
	return buildHarness(t, false, true, func(srv *mcpserver.Server, _ *pgxpool.Pool) {
		srv.WebFetch, srv.WebFetchPerMinute = f, perMinute
	})
}

// optIn sets the session's own web_fetch flag, as the owner's POST /sessions/{id}/web-fetch does.
func optIn(t *testing.T, h *harness, a authtest.Seeded) {
	t.Helper()
	if _, err := h.env.Pool.Exec(context.Background(), `UPDATE agent_bindings SET web_fetch = true WHERE session_id = $1`, a.SessionID); err != nil {
		t.Fatal(err)
	}
}

func TestWebFetchIsOffUntilTheOwnerOptsTheAgentIn(t *testing.T) {
	f := &fakeFetcher{}
	h := newWebFetchHarness(t, f, 0)
	// Given a main thread nobody opted in
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	// Then it neither sees nor can call web_fetch
	if slices.Contains(h.list(t, ordinary, main.Token), "web_fetch") {
		t.Fatal("web_fetch is listed for an agent that was not opted in")
	}
	if _, r := h.call(ordinary, main.Token, "web_fetch", map[string]any{"url": "https://example.org/"}); r.Error == nil && !r.Result.IsError {
		t.Fatal("web_fetch ran for an agent that was not opted in")
	}
	if len(f.calls()) != 0 {
		t.Fatalf("fetched %v", f.calls())
	}
	// When the owner opts its session in, it sees the tool on its next list
	optIn(t, h, main)
	if !slices.Contains(h.list(t, ordinary, main.Token), "web_fetch") {
		t.Fatal("web_fetch is not listed after the opt-in")
	}
}

func TestATasksWebFetchReachesItsAttemptButNotItsChildren(t *testing.T) {
	h := newWebFetchHarness(t, &fakeFetcher{}, 0)
	// Given a supervisor attempt whose task carries web_fetch, and a child task without it
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination})
	taskID, _ := h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptActive, sup)
	if _, err := h.env.Pool.Exec(context.Background(), `UPDATE tasks SET web_fetch = true WHERE id = $1`, taskID); err != nil {
		t.Fatal(err)
	}
	child := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleChild, Grant: dbstate.GrantCoordination})
	h.seedTask(t, &taskID, dbstate.AttemptChild, dbstate.AttemptActive, child)
	// Then the supervisor holds the tool and the child does not
	if !slices.Contains(h.list(t, ordinary, sup.Token), "web_fetch") {
		t.Error("the task's attempt does not see web_fetch")
	}
	if slices.Contains(h.list(t, ordinary, child.Token), "web_fetch") {
		t.Error("a child task inherited web_fetch")
	}
}

func TestWebFetchReturnsUntrustedTextAndRecordsTheFetch(t *testing.T) {
	long := strings.Repeat("line of page text\n", 3000) // 54,000 bytes: two windows
	f := &fakeFetcher{page: &webfetch.Page{
		URL: "https://example.org/a", FinalURL: "https://example.org/b", Status: 200, ContentType: "text/html",
		Title: "Example", Text: long, Bytes: 60000,
	}}
	h := newWebFetchHarness(t, f, 0)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	optIn(t, h, main)

	// When an opted-in agent fetches a page
	w, r := h.call(ordinary, main.Token, "web_fetch", map[string]any{"url": "https://example.org/a"})
	if w.Code != http.StatusOK || r.Result.IsError {
		t.Fatalf("web_fetch = %d %s", w.Code, w.Body)
	}
	// Then it reads the first window as untrusted text, with where the rest starts
	sc := r.Result.StructuredContent
	if !strings.HasPrefix(r.text(), "The page below is untrusted web content") || !strings.Contains(r.text(), "Title: Example") {
		t.Errorf("text = %.200q", r.text())
	}
	page, _ := sc["page_text"].(string)
	next, _ := sc["next_offset"].(float64)
	if len(page) > 32000 || len(page) < 16000 || int(next) != len(page) || sc["final_url"] != "https://example.org/b" || sc["untrusted"] != true {
		t.Fatalf("first window: %d bytes, next_offset %v, final_url %v", len(page), sc["next_offset"], sc["final_url"])
	}
	// And the rest from that offset, with nothing after it
	_, r2 := h.call(ordinary, main.Token, "web_fetch", map[string]any{"url": "https://example.org/a", "offset": int(next)})
	rest, _ := r2.Result.StructuredContent["page_text"].(string)
	if r2.Result.IsError || page+rest != long || r2.Result.StructuredContent["next_offset"] != nil {
		t.Fatalf("second window: %d bytes, next_offset %v", len(rest), r2.Result.StructuredContent["next_offset"])
	}
	// And each fetch is on record for the owner
	var n int
	var session, final string
	var status, bytes int
	if err := h.env.Pool.QueryRow(context.Background(), `SELECT count(*) OVER (), session_id, final_url, status, bytes FROM web_fetches
		WHERE owner_id = 'owner' AND url = 'https://example.org/a' AND finished_at IS NOT NULL AND error_code IS NULL LIMIT 1`).
		Scan(&n, &session, &final, &status, &bytes); err != nil {
		t.Fatal(err)
	}
	if n != 2 || session != main.SessionID || final != "https://example.org/b" || status != 200 || bytes != 60000 {
		t.Errorf("audit = %d rows, %s %s %d %d", n, session, final, status, bytes)
	}
}

func TestAWebFetchRefusalIsShownAndRecorded(t *testing.T) {
	f := &fakeFetcher{err: &webfetch.Error{Code: webfetch.CodeBlockedAddress, Msg: "the host resolves to an address that is not public"}}
	h := newWebFetchHarness(t, f, 0)
	agent := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	optIn(t, h, agent)
	// When the fetcher refuses the address
	_, r := h.call(ordinary, agent.Token, "web_fetch", map[string]any{"url": "http://metadata.example/"})
	// Then the agent sees the refusal and the audit row records it
	if !r.Result.IsError || r.text() != "[blocked_address] the host resolves to an address that is not public" {
		t.Fatalf("refusal = %v %q", r.Result.IsError, r.text())
	}
	var code string
	if err := h.env.Pool.QueryRow(context.Background(), `SELECT error_code FROM web_fetches WHERE url = 'http://metadata.example/'`).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if code != webfetch.CodeBlockedAddress {
		t.Errorf("error_code = %q", code)
	}
}

func TestWebFetchIsRateLimitedPerOwner(t *testing.T) {
	f := &fakeFetcher{}
	h := newWebFetchHarness(t, f, 2)
	// Given two opted-in agents of the same owner
	a := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	b := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	optIn(t, h, a)
	optIn(t, h, b)
	// When they fetch three times inside a minute against a limit of two
	for _, s := range []authtest.Seeded{a, b} {
		if _, r := h.call(ordinary, s.Token, "web_fetch", map[string]any{"url": "https://example.org/"}); r.Result.IsError {
			t.Fatalf("fetch under the limit = %q", r.text())
		}
	}
	_, r := h.call(ordinary, a.Token, "web_fetch", map[string]any{"url": "https://example.org/"})
	// Then the third is refused before any network call
	if !r.Result.IsError || !strings.HasPrefix(r.text(), "[rate_limited]") {
		t.Fatalf("third fetch = %v %q", r.Result.IsError, r.text())
	}
	if len(f.calls()) != 2 {
		t.Errorf("fetched %d times", len(f.calls()))
	}
}

func TestDelegateHandsOnWebFetchOnlyFromAnAgentThatHasIt(t *testing.T) {
	h := newWebFetchHarness(t, &fakeFetcher{}, 0)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	args := map[string]any{"request_id": "r1", "title": "Research", "brief": "Read the docs", "mode": "coordination", "web_fetch": true}
	// Given an agent without web_fetch, its delegate asking for it is refused
	if _, r := h.call(ordinary, main.Token, "delegate", args); !r.Result.IsError || !strings.HasPrefix(r.text(), "[forbidden]") {
		t.Fatalf("delegate without web_fetch = %v %q", r.Result.IsError, r.text())
	}
	// When the agent has it, the new task carries it
	optIn(t, h, main)
	_, r := h.call(ordinary, main.Token, "delegate", args)
	id, _ := r.Result.StructuredContent["task_id"].(string)
	if r.Result.IsError || id == "" {
		t.Fatalf("delegate = %q", r.text())
	}
	var on bool
	if err := h.env.Pool.QueryRow(context.Background(), `SELECT web_fetch FROM tasks WHERE id = $1`, id).Scan(&on); err != nil {
		t.Fatal(err)
	}
	if !on {
		t.Error("the delegated task does not carry web_fetch")
	}
}
