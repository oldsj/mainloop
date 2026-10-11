package providers_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/app/apicheck"
	"github.com/oldsj/mainloop/server/internal/app/apptest"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func parse(t *testing.T, extra map[string]string) *config.Config {
	t.Helper()
	env := map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"}
	for k, v := range extra {
		env[k] = v
	}
	cfg, err := config.Parse(env)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func profile(id, native, aliases string, enabled bool) string {
	e := "true"
	if !enabled {
		e = "false"
	}
	return `{"id":"` + id + `","display_name":"` + id + `","native_provider":"` + native + `","configuration_revision":"r1",` +
		`"agents":{"supervisor":{"namespace":"kagent","name":"` + id + `-sup"},"main":{"namespace":"kagent","name":"` + id + `-main"}},` +
		`"aliases":[` + aliases + `],"enabled":` + e + `}`
}

func TestRegistryDefaults(t *testing.T) {
	r, err := providers.NewRegistry(parse(t, nil).Kagent)
	if err != nil {
		t.Fatal(err)
	}
	ps := r.Profiles()
	if len(ps) != 2 || ps[0].ID != "claude" || ps[1].ID != "codex" {
		t.Fatalf("profiles = %+v", ps)
	}
	for _, p := range ps {
		if !p.Enabled || p.RuntimeAdapter != "kagent" || p.NativeProvider != p.ID || p.ConfigurationRevision != "native-v1" {
			t.Errorf("%s = %+v", p.ID, p)
		}
		if len(p.Capabilities) != len(providers.Capabilities) {
			t.Errorf("%s capabilities = %d", p.ID, len(p.Capabilities))
		}
		for _, c := range p.Capabilities {
			if c.State != "unknown" || c.Scope != "unverified" {
				t.Errorf("%s capability %s claims evidence: %+v", p.ID, c.Capability, c)
			}
		}
		for _, role := range []string{"main", "supervisor", "child", "agent"} {
			if p.Agents[role].Name == "" || p.Agents[role].Namespace != "kagent" {
				t.Errorf("%s agent %s = %+v", p.ID, role, p.Agents[role])
			}
		}
	}
	// Changing the returned slice does not change the registry.
	ps[0].ID = "mutated"
	if r.Profiles()[0].ID != "claude" {
		t.Error("Profiles aliases the registry")
	}
}

func TestRegistryOperatorProfiles(t *testing.T) {
	list := "[" + profile("claude", "claude", "", false) + "," + profile("team-x", "codex", `"tx"`, true) + "]"
	r, err := providers.NewRegistry(parse(t, map[string]string{"PROVIDER_PROFILES": list}).Kagent)
	if err != nil {
		t.Fatal(err)
	}
	ps := r.Profiles()
	if len(ps) != 3 || ps[0].ID != "claude" || ps[1].ID != "codex" || ps[2].ID != "team-x" {
		t.Fatalf("order = %+v", ps)
	}
	if ps[0].Enabled || ps[0].Agents["supervisor"].Name != "claude-sup" {
		t.Errorf("operator profile did not replace the built-in: %+v", ps[0])
	}

	if p, err := r.Resolve("tx", providers.RoleSupervisor, true); err != nil || p.ID != "team-x" {
		t.Errorf("alias: %+v, %v", p, err)
	}
	if _, err := r.Resolve("claude", providers.RoleSupervisor, true); !errors.Is(err, providers.ErrDisabled) {
		t.Errorf("selecting a disabled profile: %v", err)
	}
	if p, err := r.Resolve("claude", providers.RoleSupervisor, false); err != nil || p.ID != "claude" {
		t.Errorf("a disabled profile still resolves for pinned work: %+v, %v", p, err)
	}
	if _, err := r.Resolve("nope", providers.RoleSupervisor, false); !errors.Is(err, providers.ErrUnknown) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := r.Resolve("team-x", "child", false); !errors.Is(err, providers.ErrNoRole) {
		t.Errorf("missing role: %v", err)
	}
}

func TestBadConfigurationFailsClearly(t *testing.T) {
	good := profile("a", "claude", "", true)
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"bad id":           {map[string]string{"PROVIDER_PROFILES": strings.Replace("["+good+"]", `"id":"a"`, `"id":"A b"`, 1)}, "profile id"},
		"id too long":      {map[string]string{"PROVIDER_PROFILES": strings.Replace("["+good+"]", `"id":"a"`, `"id":"a`+strings.Repeat("b", 80)+`"`, 1)}, "profile id"},
		"digit first":      {map[string]string{"PROVIDER_PROFILES": strings.Replace("["+good+"]", `"id":"a"`, `"id":"1a"`, 1)}, "profile id"},
		"not json":         {map[string]string{"PROVIDER_PROFILES": "nonsense"}, "PROVIDER_PROFILES"},
		"duplicate":        {map[string]string{"PROVIDER_PROFILES": "[" + good + "," + good + "]"}, "duplicate"},
		"alias collides":   {map[string]string{"PROVIDER_PROFILES": "[" + good + "," + profile("b", "codex", `"a"`, true) + "]"}, "duplicate"},
		"reserved alias":   {map[string]string{"PROVIDER_PROFILES": "[" + profile("b", "codex", `"claude"`, true) + "]"}, "reserved"},
		"legacy id switch": {map[string]string{"PROVIDER_PROFILES": "[" + profile("claude", "codex", "", true) + "]"}, "legacy"},
		"bad agent name":   {map[string]string{"PROVIDER_PROFILES": strings.Replace("["+good+"]", `a-sup`, `Bad Name`, 1)}, "AgentRef"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.Parse(mergeEnv(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	// A bad KAGENT_* agent name is caught by the registry, because the built-ins use it.
	cfg := parse(t, nil)
	cfg.Kagent.SupervisorClaudeAgent = "Not Valid"
	if _, err := providers.NewRegistry(cfg.Kagent); err == nil || !strings.Contains(err.Error(), "AgentRef") {
		t.Errorf("NewRegistry with a bad built-in agent: %v", err)
	}

}

func mergeEnv(extra map[string]string) map[string]string {
	env := map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

type harness struct {
	t       *testing.T
	db      *testdb.DB
	handler http.Handler
}

func newHarness(t *testing.T, extra map[string]string) *harness {
	t.Helper()
	d := testdb.New(t)
	h, err := app.New(apptest.Deps(t, parse(t, extra), discard(), d.Pool)).Handler()
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, db: d, handler: apicheck.Wrap(t, h)}
}

type reply struct {
	status int
	body   string
}

func (r reply) obj() map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		panic(r.body)
	}
	return m
}

func (h *harness) do(method, path string, body any) reply {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, rd)
	req.Host = "api.test"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return reply{rec.Code, rec.Body.String()}
}

func (h *harness) want(r reply, status int) reply {
	h.t.Helper()
	if r.status != status {
		h.t.Fatalf("status = %d, want %d; body %s", r.status, status, r.body)
	}
	return r
}

func (h *harness) project(id, user string) {
	h.t.Helper()
	_, err := h.db.Pool.Exec(context.Background(),
		`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ($1, $2, 'o', $1, 'o/'||$1, 'https://example.test/'||$1)`, id, user)
	if err != nil {
		h.t.Fatal(err)
	}
}

func TestListProviders(t *testing.T) {
	list := "[" + profile("team-x", "codex", `"tx"`, false) + "]"
	h := newHarness(t, map[string]string{"PROVIDER_PROFILES": list})
	var got []map[string]any
	if err := json.Unmarshal([]byte(h.want(h.do("GET", "/providers", nil), 200).body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0]["id"] != "claude" || got[1]["id"] != "codex" || got[2]["id"] != "team-x" {
		t.Fatalf("providers = %v", got)
	}
	x := got[2]
	if x["enabled"] != false || x["native_provider"] != "codex" || x["runtime_adapter"] != "kagent" || x["configuration_revision"] != "r1" ||
		x["display_name"] != "team-x" || len(x["aliases"].([]any)) != 1 {
		t.Errorf("team-x = %v", x)
	}
	agents := x["agents"].(map[string]any)
	if a := agents["supervisor"].(map[string]any); a["name"] != "team-x-sup" || a["namespace"] != "kagent" {
		t.Errorf("agents = %v", agents)
	}
	// Builtins report every capability with no evidence.
	caps := got[0]["capabilities"].([]any)
	if len(caps) != len(providers.Capabilities) {
		t.Fatalf("capabilities = %d", len(caps))
	}
	if c := caps[0].(map[string]any); c["capability"] != "native_create" || c["state"] != "unknown" || c["scope"] != "unverified" {
		t.Errorf("capability = %v", c)
	}
}

func TestDefaultProviderCompareAndSwap(t *testing.T) {
	h := newHarness(t, map[string]string{"PROVIDER_PROFILES": "[" + profile("off", "codex", "", false) + "]"})
	h.project("p1", "local-dev-user")
	h.project("foreign", "someone-else")
	put := func(project string, profileID any, version int) reply {
		return h.do("PUT", "/projects/"+project+"/default-provider", map[string]any{"profile_id": profileID, "expected_version": version})
	}

	if r := h.want(h.do("GET", "/projects/p1/default-provider", nil), 200).obj(); r["profile_id"] != nil || r["version"] != 0.0 || r["project_id"] != "p1" {
		t.Errorf("empty preference = %v", r)
	}

	if r := h.want(put("p1", "claude", 0), 200).obj(); r["profile_id"] != "claude" || r["version"] != 1.0 {
		t.Errorf("first write = %v", r)
	}
	if r := h.want(h.do("GET", "/projects/p1/default-provider", nil), 200).obj(); r["profile_id"] != "claude" || r["version"] != 1.0 {
		t.Errorf("read = %v", r)
	}

	// A second writer that still believes the preference is unset loses.
	r := h.want(put("p1", "codex", 0), 409).obj()
	if d, _ := r["detail"].(map[string]any); d["reason"] != "stale_provider_preference" {
		t.Errorf("stale body = %v", r)
	}
	h.want(put("p1", "codex", 7), 409)
	if r := h.want(h.do("GET", "/projects/p1/default-provider", nil), 200).obj(); r["profile_id"] != "claude" || r["version"] != 1.0 {
		t.Errorf("a lost write changed the preference: %v", r)
	}

	if r := h.want(put("p1", "codex", 1), 200).obj(); r["profile_id"] != "codex" || r["version"] != 2.0 {
		t.Errorf("update = %v", r)
	}
	// Clearing is a write too.
	if r := h.want(put("p1", nil, 2), 200).obj(); r["profile_id"] != nil || r["version"] != 3.0 {
		t.Errorf("clear = %v", r)
	}

	// Unknown, disabled and mistyped profiles are rejected before anything is stored.
	for _, id := range []string{"nope", "off", ""} {
		h.want(put("p1", id, 3), 422)
	}
	h.want(h.do("PUT", "/projects/p1/default-provider", map[string]any{"profile_id": "claude"}), 422)
	if r := h.want(h.do("GET", "/projects/p1/default-provider", nil), 200).obj(); r["version"] != 3.0 {
		t.Errorf("rejected writes changed the version: %v", r)
	}

	// Another owner's project and a missing one look the same.
	for _, id := range []string{"foreign", "missing"} {
		for _, r := range []reply{h.do("GET", "/projects/"+id+"/default-provider", nil), put(id, "claude", 0)} {
			h.want(r, 404)
			if d, _ := r.obj()["detail"].(map[string]any); d["reason"] != "project_not_found" {
				t.Errorf("%s: body = %s", id, r.body)
			}
		}
	}
	var n int
	if err := h.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM project_provider_preferences WHERE project_id IN ('foreign','missing')`).Scan(&n); err != nil || n != 0 {
		t.Errorf("preferences stored for unowned projects: %d, %v", n, err)
	}
}

func TestDefaultProviderConcurrentWritersHaveOneWinner(t *testing.T) {
	h := newHarness(t, nil)
	h.project("p1", "local-dev-user")
	for round := range 5 {
		statuses := make([]int, 4)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range statuses {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				id := []string{"claude", "codex"}[i%2]
				statuses[i] = h.do("PUT", "/projects/p1/default-provider", map[string]any{"profile_id": id, "expected_version": round}).status
			}()
		}
		close(start)
		wg.Wait()
		ok := 0
		for _, s := range statuses {
			switch s {
			case 200:
				ok++
			case 409:
			default:
				t.Fatalf("round %d: statuses = %v", round, statuses)
			}
		}
		if ok != 1 {
			t.Fatalf("round %d: %d writers won, want 1 (%v)", round, ok, statuses)
		}
	}
	if r := h.do("GET", "/projects/p1/default-provider", nil).obj(); r["version"] != 5.0 {
		t.Errorf("version = %v", r["version"])
	}
}

func TestTheMainRoleResolvesPerProvider(t *testing.T) {
	cfg := parse(t, map[string]string{"KAGENT_MAIN_AGENT": "claude-main", "KAGENT_MAIN_CODEX_AGENT": "codex-main"})
	reg, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		t.Fatal(err)
	}
	for provider, want := range map[string]string{"claude": "claude-main", "codex": "codex-main"} {
		p, err := reg.Resolve(provider, "main", true)
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		if got := p.Agents["main"].Name; got != want {
			t.Errorf("%s main Agent = %q, want %q", provider, got, want)
		}
	}
}
