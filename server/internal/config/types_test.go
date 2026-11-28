package config_test

import (
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/config"
)

// MAINLOOP_LOCKED_AUTO_AGENTS lists Agents as name or namespace/name; anything else, or a repeat,
// stops the process instead of widening the declaration.
func TestLockedAutoAgents(t *testing.T) {
	base := map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"}
	with := func(v string) map[string]string {
		m := map[string]string{"MAINLOOP_LOCKED_AUTO_AGENTS": v}
		for k, x := range base {
			m[k] = x
		}
		return m
	}
	cfg, err := config.Parse(with(" claude-main-dev, kagent/claude-supervisor-dev ,"))
	if err != nil {
		t.Fatal(err)
	}
	want := config.AgentRefs{{Name: "claude-main-dev"}, {Namespace: "kagent", Name: "claude-supervisor-dev"}}
	if len(cfg.Kagent.LockedAutoAgents) != 2 || cfg.Kagent.LockedAutoAgents[0] != want[0] || cfg.Kagent.LockedAutoAgents[1] != want[1] {
		t.Fatalf("agents = %+v", cfg.Kagent.LockedAutoAgents)
	}
	if cfg, err := config.Parse(base); err != nil || len(cfg.Kagent.LockedAutoAgents) != 0 {
		t.Fatalf("default = %+v, %v", cfg.Kagent.LockedAutoAgents, err)
	}
	for _, bad := range []string{"Claude", "a/b/c", "/x", "ns/", "a,a", "kagent/a,kagent/a", "has space"} {
		if _, err := config.Parse(with(bad)); err == nil || !strings.Contains(err.Error(), "MAINLOOP_LOCKED_AUTO_AGENTS") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}
