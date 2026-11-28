package sessions_test

import (
	"context"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

func (e *env) setDefault(role, provider string, model *string, effort *api.EffortLevel) (api.SessionDefaults, error) {
	e.t.Helper()
	resp, err := e.API().SetSessionDefault(context.Background(), api.SetSessionDefaultRequestObject{Body: &api.SessionDefault{
		Role: api.SessionDefaultRole(role), Provider: provider, Model: model, Effort: effort,
	}})
	if err != nil {
		return api.SessionDefaults{}, err
	}
	return api.SessionDefaults(resp.(api.SetSessionDefault200JSONResponse)), nil
}

// A turn names the owner's default for its role and provider when the session has no override of
// its own: the override wins, a reset falls back to the default, a change reaches the running
// session from its next turn, and the main thread's default is not a worker's.
func TestTurnsUseTheRoleDefaultUnderTheSessionOverride(t *testing.T) {
	var log metaLog
	e := newEnv(t, metaOpt(t, &log))
	e.withProviders()
	if _, err := e.setDefault("worker", "claude", strp("claude-opus-5-5"), effortp("high")); err != nil {
		t.Fatalf("set worker default: %v", err)
	}
	if _, err := e.setDefault("main", "claude", nil, effortp("medium")); err != nil {
		t.Fatalf("set main default: %v", err)
	}
	id, err := e.svc.Create(context.Background(), sessions.NewSession{Title: "Work", Description: "d", Prompt: "start", AgentKind: "claude"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	wantMeta(t, "first turn", log.last(t), "claude-opus-5-5", "high")

	if _, err := e.setEffort(id, effortp("low")); err != nil {
		t.Fatalf("set effort: %v", err)
	}
	e.sendTurn(id, "override")
	wantMeta(t, "override wins", log.last(t), "claude-opus-5-5", "low")

	if _, err := e.setEffort(id, nil); err != nil {
		t.Fatalf("reset effort: %v", err)
	}
	if _, err := e.setDefault("worker", "claude", strp("claude-sonnet-5-5"), effortp("high")); err != nil {
		t.Fatalf("change worker default: %v", err)
	}
	e.sendTurn(id, "reset")
	wantMeta(t, "reset reads the current default", log.last(t), "claude-sonnet-5-5", "high")

	// A Codex session has no default: the Claude one does not apply to another provider.
	cid, err := e.svc.Create(context.Background(), sessions.NewSession{Title: "Codex", Description: "d", Prompt: "start", AgentKind: "codex"})
	if err != nil {
		t.Fatalf("create codex: %v", err)
	}
	e.run()
	e.waitState(e.firstDelivery(cid), dbstate.DeliveryCompleted)
	wantMeta(t, "other provider", log.last(t), "", "")

	// The main thread takes the main default.
	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatalf("ensure main: %v", err)
	}
	e.run()
	e.sendTurn(m.SessionID, "hello main")
	wantMeta(t, "main thread", log.last(t), "", "medium")

	// Clearing both fields removes the default.
	got, err := e.setDefault("worker", "claude", nil, nil)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if len(got.Defaults) != 1 || got.Defaults[0].Role != "main" || got.Defaults[0].Model != nil ||
		got.Defaults[0].Effort == nil || *got.Defaults[0].Effort != "medium" {
		t.Fatalf("defaults after clearing = %+v", got.Defaults)
	}
	e.sendTurn(id, "no default")
	wantMeta(t, "cleared", log.last(t), "", "")
}

// A supervisor (a task without a parent) takes the supervisor default, field by field, then the
// worker default; the override still wins; a worker never takes the supervisor's.
func TestSupervisorTurnsFallBackFromSupervisorToWorkerDefaults(t *testing.T) {
	var log metaLog
	e := newEnv(t, metaOpt(t, &log))
	e.withProviders()
	ctx := context.Background()
	sup := e.newSession("plan")
	worker := e.newSession("code")
	e.run()
	e.waitState(e.firstDelivery(sup), dbstate.DeliveryCompleted)
	if _, err := e.pool.Exec(ctx, `UPDATE agent_bindings SET role = 'supervisor' WHERE session_id = $1`, sup); err != nil {
		t.Fatal(err)
	}
	e.waitState(e.firstDelivery(worker), dbstate.DeliveryCompleted)

	// With only a worker default, a supervisor uses it: owners who never set supervisor see no change.
	if _, err := e.setDefault("worker", "claude", strp("claude-sonnet-5-5"), effortp("high")); err != nil {
		t.Fatal(err)
	}
	e.sendTurn(sup, "worker fallback")
	wantMeta(t, "supervisor, worker default only", log.last(t), "claude-sonnet-5-5", "high")

	// A supervisor default wins over the worker's, and the worker keeps its own.
	if _, err := e.setDefault("supervisor", "claude", strp("claude-opus-5-5"), effortp("medium")); err != nil {
		t.Fatal(err)
	}
	e.sendTurn(sup, "supervisor default")
	wantMeta(t, "supervisor default", log.last(t), "claude-opus-5-5", "medium")
	e.sendTurn(worker, "worker turn")
	wantMeta(t, "worker unaffected", log.last(t), "claude-sonnet-5-5", "high")

	// The session's own override wins over both.
	if _, err := e.setEffort(sup, effortp("low")); err != nil {
		t.Fatal(err)
	}
	e.sendTurn(sup, "override")
	wantMeta(t, "override wins", log.last(t), "claude-opus-5-5", "low")

	// A field the supervisor default leaves unset takes the worker's.
	if _, err := e.setEffort(sup, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.setDefault("supervisor", "claude", strp("claude-opus-5-5"), nil); err != nil {
		t.Fatal(err)
	}
	e.sendTurn(sup, "partial")
	wantMeta(t, "effort falls back to worker", log.last(t), "claude-opus-5-5", "high")

	// Removing the supervisor default returns to the worker's.
	if _, err := e.setDefault("supervisor", "claude", nil, nil); err != nil {
		t.Fatal(err)
	}
	e.sendTurn(sup, "removed")
	wantMeta(t, "supervisor default removed", log.last(t), "claude-sonnet-5-5", "high")
}

func TestSetSessionDefaultRefusesWhatTheProviderDoesNotOffer(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	if _, err := e.setDefault("worker", "codex", nil, effortp("max")); err == nil {
		t.Fatal("codex accepted effort max, which it does not offer")
	}
	if _, err := e.setDefault("worker", "nope", strp("m"), nil); err == nil {
		t.Fatal("an unknown provider was accepted")
	}
	if _, err := e.setDefault("boss", "claude", strp("m"), nil); err == nil {
		t.Fatal("an unknown role was accepted")
	}
}
