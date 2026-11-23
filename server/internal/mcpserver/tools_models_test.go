package mcpserver_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// TestListModelsGivesTheIDsDelegateAccepts: list_models lists each provider a delegate may name
// with its exact model IDs and effort levels, and a delegate that names them is accepted; a
// guessed short name is refused as before.
func TestListModelsGivesTheIDsDelegateAccepts(t *testing.T) {
	// Given main, on an installation whose default provider is claude
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})

	// When it lists the models
	_, r := h.call(ordinary, main.Token, "list_models", map[string]any{})
	if r.Result.IsError {
		t.Fatalf("list_models = %q", r.text())
	}

	// Then claude is listed as the default, for both task roles, with its models and effort levels
	var claude map[string]any
	for _, p := range r.Result.StructuredContent["providers"].([]any) {
		if p := p.(map[string]any); p["provider_profile_id"] == "claude" {
			claude = p
		}
	}
	if claude == nil || claude["default"] != true || fmt.Sprint(claude["roles"]) != "[supervisor child]" {
		t.Fatalf("claude = %v", claude)
	}
	models, efforts := claude["models"].([]any), claude["effort_levels"].([]any)
	if len(models) == 0 || len(efforts) == 0 {
		t.Fatalf("claude offers models %v and efforts %v", models, efforts)
	}
	model, effort := models[0].(map[string]any)["id"].(string), efforts[0].(string)
	if !strings.Contains(r.text(), "claude (default provider), roles supervisor, child") || !strings.Contains(r.text(), model) {
		t.Errorf("text = %q", r.text())
	}

	// And a delegate naming them exactly is accepted
	_, d := h.call(ordinary, main.Token, "delegate", map[string]any{
		"request_id": "listed", "title": "x", "brief": "y", "mode": "coordination",
		"provider_profile_id": "claude", "model": model, "effort": effort,
	})
	if d.Result.IsError || d.Result.StructuredContent["kind"] != "create" {
		t.Fatalf("delegate with listed IDs = %q", d.text())
	}

	// While a guessed short name is still refused, with the allowed list
	_, g := h.call(ordinary, main.Token, "delegate", map[string]any{
		"request_id": "guessed", "title": "x", "brief": "y", "mode": "coordination", "provider_profile_id": "claude", "model": "haiku",
	})
	if !g.Result.IsError || !strings.HasPrefix(g.text(), "[422] model_unavailable") {
		t.Fatalf("delegate with a guessed name = %q", g.text())
	}
}

// TestListModelsTakesNoArguments: it is a read with no input; anything given is refused.
func TestListModelsTakesNoArguments(t *testing.T) {
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	if _, r := h.call(ordinary, main.Token, "list_models", map[string]any{"provider": "claude"}); !r.Result.IsError {
		t.Fatalf("list_models with an argument = %q", r.text())
	}
}
