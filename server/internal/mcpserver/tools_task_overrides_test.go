package mcpserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/providers"
)

func TestDelegateInitialOverrides(t *testing.T) {
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k"})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := registry.Profile("claude")
	if !ok || len(profile.Models) == 0 || len(profile.EffortLevels) == 0 {
		t.Fatal("test profile has no override choices")
	}
	args := map[string]any{
		"request_id": "overrides", "title": "Plan", "brief": "Plan it", "mode": "coordination",
		"model": profile.Models[0].ID, "effort": profile.EffortLevels[0],
	}
	_, result := h.call(ordinary, main.Token, "delegate", args)
	if result.Result.IsError {
		t.Fatal(result.text())
	}
	var model, effort string
	if err := h.env.Pool.QueryRow(context.Background(), `SELECT request_payload->>'model', request_payload->>'effort' FROM task_operations WHERE id = $1`, result.Result.StructuredContent["id"]).Scan(&model, &effort); err != nil {
		t.Fatal(err)
	}
	if model != profile.Models[0].ID || effort != profile.EffortLevels[0] {
		t.Fatalf("stored overrides = %s, %s", model, effort)
	}
	args["request_id"], args["model"], args["effort"] = "invalid-overrides", "not-offered", "not-offered"
	_, result = h.call(ordinary, main.Token, "delegate", args)
	if !result.Result.IsError || !strings.Contains(result.text(), "[422]") ||
		!strings.Contains(result.text(), "allowed models:") || !strings.Contains(result.text(), "allowed effort_levels:") ||
		!strings.Contains(result.text(), profile.Models[0].ID) {
		t.Fatalf("invalid overrides = %q", result.text())
	}
}
