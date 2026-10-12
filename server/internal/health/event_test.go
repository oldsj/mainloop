package health_test

import (
	"sync"
	"testing"

	"github.com/oldsj/mainloop/server/internal/health"
)

// Python: test_lexical_paths_and_unknown_fields.
func TestSafePath(t *testing.T) {
	for p, want := range map[string]bool{
		"src/app.ts": true, "a/b/c.go": true, ".github/workflows/ci.yml": true,
		"": false, "/etc/passwd": false, "../x": false, "a/../b": false, "a/./b": false, "a//b": false, ".": false,
		"a\nb": false, "a\x00b": false, "a?b": false, "a@b": false, `a\b`: false,
		".env": false, "config/.env.local": false, "my-credentials.json": false, "app/secret.txt": false,
		".ssh": false, "home/.aws": false, ".kube": false, "id.pem": false, "tls.key": false,
	} {
		if got := health.SafePath(p); got != want {
			t.Errorf("SafePath(%q) = %v, want %v", p, got, want)
		}
	}
}

func event(over map[string]any) map[string]any {
	e := map[string]any{
		"schema_version": health.Schema, "event_id": "22222222-2222-4222-8222-222222222222:1",
		"producer_epoch": "22222222-2222-4222-8222-222222222222", "sequence": float64(1), "kind": "turn",
		"occurred_at": "2026-10-10T12:00:00Z", "time_source": "adapter_observed", "time_precision": "nanosecond",
		"provider": "claude", "runtime_version": "2.1.260", "source": "claude_result_stream",
		"turn_key": "22222222-2222-4222-8222-222222222222", "runtime_session_id": runtimeSession, "a2a_task_id": "t",
		"turn": map[string]any{"state": "started", "coverage": "partial"},
	}
	for k, v := range over {
		if v == nil {
			delete(e, k)
		} else {
			e[k] = v
		}
	}
	return e
}

func TestParseEvent(t *testing.T) {
	if _, err := health.ParseEvent(event(nil)); err != nil {
		t.Fatalf("a valid event: %v", err)
	}
	for name, over := range map[string]map[string]any{
		"extra key":           {"extra": 1},
		"missing key":         {"runtime_version": nil},
		"wrong kind":          {"kind": "other"},
		"event id not epoch":  {"event_id": "x:1"},
		"turn key not epoch":  {"turn_key": "33333333-3333-4333-8333-333333333333"},
		"sequence zero":       {"sequence": float64(0)},
		"sequence too big":    {"sequence": float64(1000001)},
		"sequence fractional": {"sequence": 1.5},
		"timestamp no zone":   {"occurred_at": "2026-10-10T12:00:00"},
		"unknown provider":    {"provider": "gpt"},
		"turn has tool":       {"tool": map[string]any{}},
		"long runtime":        {"runtime_version": "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
	} {
		if _, err := health.ParseEvent(event(over)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A part is a health part by its artifact name or by a schema_version of the family in its data
	// (so a future version becomes a recorded gap); anything else is a business artifact.
	family := map[string]any{"schema_version": "kagent.dev/orchestration-health/v2"}
	if !health.IsHealthPart("kagent.dev/orchestration-health/v1", map[string]any{}) || !health.IsHealthPart("business-result", family) ||
		health.IsHealthPart("business-result", map[string]any{"schema_version": "other/v1"}) || health.IsHealthPart("business-result", "text") {
		t.Error("IsHealthPart selects the wrong parts")
	}
}

func TestCanonicalIgnoresKeyOrderAndSpacing(t *testing.T) {
	a, err1 := health.Canonical([]byte(`{"b":1,"a":{"d":[1,2],"c":"x"}}`))
	b, err2 := health.Canonical([]byte(`{ "a": {"c":"x", "d":[1,2]}, "b": 1 }`))
	c, err3 := health.Canonical([]byte(`{"a":{"c":"x","d":[2,1]},"b":1}`))
	if err1 != nil || err2 != nil || err3 != nil || string(a) != string(b) || string(a) == string(c) {
		t.Fatalf("%s | %s | %s (%v %v %v)", a, b, c, err1, err2, err3)
	}
}

// Two collectors racing on one task store it once, with no error.
func TestConcurrentIngestOfOneTask(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	results := make([]health.Result, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = e.ingest(t, e.tasks[task1], nil)
		}()
	}
	wg.Wait()
	if results[0].Stored+results[1].Stored != 9 || e.count(t, "health_events") != 9 || e.count(t, "health_turns") != 1 {
		t.Fatalf("%+v %d events", results, e.count(t, "health_events"))
	}
}
