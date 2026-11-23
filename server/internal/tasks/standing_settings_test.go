package tasks_test

import (
	"context"
	"testing"

	"github.com/oldsj/mainloop/server/internal/tasks"
)

func sp(s string) *string { return &s }

func choice(provider, model, effort *string) tasks.SupervisorChoice {
	return tasks.SupervisorChoice{ProviderProfileID: provider, Model: model, Effort: effort}
}

func TestProjectSupervisorSettingsStartAndChangeTheSupervisor(t *testing.T) {
	r := newRig(t)
	r.run()
	ctx := context.Background()

	// Given the owner chooses a model and effort for p1's supervisor before it has one
	v, err := r.svc.SetSupervisorSettings(ctx, owner, "p1", choice(nil, sp("claude-opus-5-5"), sp("high")), tasks.LimitChanger{})
	if err != nil {
		t.Fatal(err)
	}
	if v.EffectiveProfileID != "claude" || v.OperationID != nil || len(v.AllowedModels) == 0 {
		t.Fatalf("set = %+v", v)
	}

	// When the supervisor starts, Then its first session runs on them
	id, session, _ := r.runningStanding()
	r.eventually("supervisor provisioned", func() bool { return r.opState(id) == "completed" })
	attempt, _ := r.attemptOf(id)
	if got := r.str(`SELECT model || '/' || effort FROM task_attempts WHERE id = $1`, attempt); got != "claude-opus-5-5/high" {
		t.Fatalf("attempt = %q", got)
	}
	if got := r.str(`SELECT model_override || '/' || effort_override FROM agent_bindings WHERE session_id = $1`, session); got != "claude-opus-5-5/high" {
		t.Fatalf("session override = %q", got)
	}

	// When the owner lowers the effort and returns the model to the default
	if _, err := r.svc.SetSupervisorSettings(ctx, owner, "p1", choice(nil, nil, sp("low")), tasks.LimitChanger{}); err != nil {
		t.Fatal(err)
	}

	// Then the running session takes it at once, and the task carries it for its next attempt
	if n := r.count(`SELECT count(*) FROM agent_bindings WHERE session_id = $1 AND model_override IS NULL AND effort_override = 'low'`, session); n != 1 {
		t.Fatal("the running session kept its old override")
	}
	if n := r.count(`SELECT count(*) FROM tasks WHERE id = $1 AND model IS NULL AND effort = 'low'`, id); n != 1 {
		t.Fatal("the task kept its old model and effort")
	}

	// And setting the same again changes nothing
	if _, err := r.svc.SetSupervisorSettings(ctx, owner, "p1", choice(nil, nil, sp("low")), tasks.LimitChanger{}); err != nil {
		t.Fatal(err)
	}
	if n := r.count(`SELECT count(*) FROM project_supervisor_settings_audit WHERE project_id = 'p1'`); n != 2 {
		t.Fatalf("audit rows = %d, want 2", n)
	}

	// When the owner moves it to another provider
	v, err = r.svc.SetSupervisorSettings(ctx, owner, "p1", choice(sp("codex"), nil, nil), tasks.LimitChanger{})
	if err != nil {
		t.Fatal(err)
	}

	// Then it is handed off to that provider on the same task
	if v.OperationID == nil || v.EffectiveProfileID != "codex" {
		t.Fatalf("provider change = %+v", v)
	}
	r.handedOff(id)
	next, _ := r.attemptOf(id)
	if got := r.str(`SELECT profile_id FROM task_attempts WHERE id = $1`, next); next == attempt || got != "codex" {
		t.Fatalf("successor %s on %q", next, got)
	}
	if got := r.str(`SELECT coalesce(operation_id, '') FROM project_supervisor_settings_audit ORDER BY id DESC LIMIT 1`); got != *v.OperationID {
		t.Fatalf("audit operation = %q", got)
	}

	// And reading the settings shows the project's choice and what it resolves to
	got, err := r.svc.SupervisorSettings(ctx, owner, "p1")
	if err != nil || got.ProviderProfileID == nil || *got.ProviderProfileID != "codex" || got.EffectiveProfileID != "codex" || got.Effort != nil {
		t.Fatalf("read = %+v, %v", got, err)
	}
}

func TestProjectSupervisorSettingsRefuseWhatTheProviderDoesNotOffer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Given a model and an effort the resolved provider does not offer, Then each is refused as
	// delegate refuses it, and nothing is recorded
	_, err := e.svc.SetSupervisorSettings(ctx, owner, "p1", choice(nil, sp("gpt-6.1-sol"), nil), tasks.LimitChanger{})
	wantRefusal(t, err, 422, tasks.ReasonModelUnavailable)
	_, err = e.svc.SetSupervisorSettings(ctx, owner, "p1", choice(nil, nil, sp("minimal")), tasks.LimitChanger{})
	wantRefusal(t, err, 422, tasks.ReasonEffortUnavailable)
	_, err = e.svc.SetSupervisorSettings(ctx, owner, "p1", choice(sp("nope"), nil, nil), tasks.LimitChanger{})
	if err == nil {
		t.Fatal("an unknown provider was accepted")
	}
	if n := e.count(`SELECT count(*) FROM project_supervisor_settings_audit`); n != 0 {
		t.Fatalf("audit rows = %d", n)
	}

	// And another owner's project, or none, is not found
	for _, p := range []string{"foreign", "missing"} {
		_, err := e.svc.SetSupervisorSettings(ctx, owner, p, choice(nil, nil, sp("high")), tasks.LimitChanger{})
		wantRefusal(t, err, 404, tasks.ReasonProjectNotFound)
		_, err = e.svc.SupervisorSettings(ctx, owner, p)
		wantRefusal(t, err, 404, tasks.ReasonProjectNotFound)
	}

	// And main's change records its session
	if _, err := e.svc.SetSupervisorSettings(ctx, owner, "p1", choice(nil, nil, sp("high")), tasks.LimitChanger{SessionID: "main-1"}); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM project_supervisor_settings_audit WHERE changed_by = 'main' AND session_id = 'main-1'`); n != 1 {
		t.Fatalf("main's audit rows = %d", n)
	}
}

func TestAStandingSupervisorsProviderDoesNotBindItsWorkers(t *testing.T) {
	r := newRig(t)
	r.run()

	// Given p1's supervisor runs on a provider the project chose for it
	if _, err := r.svc.SetSupervisorSettings(context.Background(), owner, "p1", choice(sp("claude"), nil, nil), tasks.LimitChanger{}); err != nil {
		t.Fatal(err)
	}
	id, _, sup := r.runningStanding()
	if n := r.count(`SELECT count(*) FROM tasks WHERE id = $1 AND provider_constraint IS NULL AND selection_source = 'explicit'`, id); n != 1 {
		t.Fatal("the standing supervisor's provider is a constraint")
	}

	// When it delegates to another provider, Then the worker runs there
	child := r.mustCreate(sup, tasks.CreateInput{Brief: "look into it", ProviderProfileID: "codex"})
	if got := r.str(`SELECT assigned_profile_id FROM tasks WHERE id = $1`, child); got != "codex" {
		t.Fatalf("worker provider = %q", got)
	}
}

func TestTheOwnerSetsSupervisorSettingsOverREST(t *testing.T) {
	r := newRest(t)

	// Given a project on the defaults
	got := r.do("GET", "/projects/p1/supervisor/settings", nil, 200)
	if got["provider_profile_id"] != nil || got["model"] != nil || got["effort"] != nil || got["effective_provider_profile_id"] != "claude" || got["operation_id"] != nil {
		t.Fatalf("unset = %v", got)
	}

	// When the owner sets a model and effort, Then the answer carries them
	got = r.do("PUT", "/projects/p1/supervisor/settings", map[string]any{"provider_profile_id": nil, "model": "claude-opus-5-5", "effort": "high"}, 200)
	if got["model"] != "claude-opus-5-5" || got["effort"] != "high" || got["effective_provider_profile_id"] != "claude" {
		t.Fatalf("set = %v", got)
	}

	// And a model the provider does not offer, a missing field or an unknown project is refused
	r.do("PUT", "/projects/p1/supervisor/settings", map[string]any{"provider_profile_id": nil, "model": "gpt-6.1-sol", "effort": nil}, 422)
	r.do("PUT", "/projects/p1/supervisor/settings", map[string]any{"model": nil, "effort": nil}, 422)
	r.do("PUT", "/projects/missing/supervisor/settings", map[string]any{"provider_profile_id": nil, "model": nil, "effort": nil}, 404)
	r.do("GET", "/projects/foreign/supervisor/settings", nil, 404)
}
