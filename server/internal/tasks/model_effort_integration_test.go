package tasks_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/tasks"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

func overrideProfile(t *testing.T, r *rig, id string) config.ProviderProfile {
	t.Helper()
	profile, err := r.svc.Providers.Resolve(id, "supervisor", false)
	if err != nil || len(profile.Models) < 2 || len(profile.EffortLevels) < 2 {
		t.Fatalf("profile %q needs multiple model and effort choices: %+v", id, profile)
	}
	return profile
}

func TestTaskModelEffortValidation(t *testing.T) {
	for _, selection := range []string{"explicit", "project_default", "owner_default", "inherited"} {
		for _, invalid := range []string{"model", "effort", "both", "empty_model", "empty_effort"} {
			t.Run(selection+"/"+invalid, func(t *testing.T) {
				r := newRig(t)
				profile := overrideProfile(t, r, "codex")
				in := tasks.CreateInput{ProjectID: "p1"}
				principal := r.own
				switch selection {
				case "explicit":
					in.ProviderProfileID = profile.ID
				case "project_default":
					r.exec(`INSERT INTO project_provider_preferences (project_id, profile_id, version) VALUES ('p1', $1, 1)`, profile.ID)
				case "owner_default":
					r.svc.DefaultProfile = profile.ID
				case "inherited":
					r.run()
					parent := r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1", ProviderProfileID: profile.ID})
					r.eventually("parent provisioned", func() bool { return r.opState(parent) == "completed" })
					_, binding := r.attemptOf(parent)
					principal = r.agent(dbstate.RoleSupervisor, binding)
				}
				in.Model, in.Effort = ptr(profile.Models[0].ID), ptr(profile.EffortLevels[0])
				if invalid == "model" || invalid == "both" {
					in.Model = ptr("not-an-offered-model")
				}
				if invalid == "effort" || invalid == "both" {
					in.Effort = ptr("not-an-offered-effort")
				}
				if invalid == "empty_model" {
					in.Model = ptr("")
				}
				if invalid == "empty_effort" {
					in.Effort = ptr("")
				}
				before := map[string]int{}
				for _, table := range []string{"tasks", "task_operations", "task_attempts", "sessions", "agent_bindings", "workspaces"} {
					before[table] = r.count(`SELECT count(*) FROM ` + table)
				}
				_, err := r.create(principal, in)
				wantRefusal(t, err, 422, tasks.ReasonInvalidRequest)
				var refusal *tasks.Error
				if !errors.As(err, &refusal) || !strings.Contains(refusal.Detail, profile.ID) {
					t.Fatalf("error detail does not identify resolved profile: %v", err)
				}
				if strings.Contains(invalid, "model") || invalid == "both" {
					for _, offered := range profile.Models {
						if !strings.Contains(refusal.Detail, offered.ID) {
							t.Errorf("detail %q omits allowed model %q", refusal.Detail, offered.ID)
						}
					}
				}
				if strings.Contains(invalid, "effort") || invalid == "both" {
					for _, offered := range profile.EffortLevels {
						if !strings.Contains(refusal.Detail, offered) {
							t.Errorf("detail %q omits allowed effort %q", refusal.Detail, offered)
						}
					}
				}
				for table, count := range before {
					if got := r.count(`SELECT count(*) FROM ` + table); got != count {
						t.Errorf("invalid create changed %s rows from %d to %d", table, count, got)
					}
				}
			})
		}
	}
}

type overrideWorkspaces struct {
	*fakeWorkspaces
	client *kagent.Client
}

func (w *overrideWorkspaces) CreateWithRequestID(ctx context.Context, requestID string, body api.CreateWorkspaceRequest) (api.WorkspaceLifecycle, error) {
	workspace, err := w.fakeWorkspaces.CreateWithRequestID(ctx, requestID, body)
	if err != nil {
		return workspace, err
	}
	native, err := w.client.CreateSession(ctx, &kagentapi.CreateSessionRequest{RequestId: requestID, Agent: &kagentapi.ResourceReference{Namespace: "kagent", Name: "agent"}})
	if err != nil {
		return workspace, err
	}
	_, err = w.pool.Exec(ctx, `UPDATE agent_bindings SET kagent_session_id = $2 WHERE session_id = $1`, workspace.SessionId, native.GetId())
	return workspace, err
}

func TestTaskModelEffortFirstNativeTurn(t *testing.T) {
	for _, profileID := range []string{"claude", "codex"} {
		for _, mode := range []dbstate.TaskMode{dbstate.TaskModeCode, dbstate.TaskModeCoordination} {
			for _, overrides := range []string{"both", "model", "effort", "nil"} {
				t.Run(profileID+"/"+string(mode)+"/"+overrides, func(t *testing.T) {
					r := newRig(t)
					profile := overrideProfile(t, r, profileID)
					in := tasks.CreateInput{ProjectID: "p1", Mode: mode, ProviderProfileID: profile.ID}
					if mode == dbstate.TaskModeCode {
						in.Checkout = &tasks.Checkout{Branch: "overrides"}
					}
					if overrides == "both" || overrides == "model" {
						in.Model = ptr(profile.Models[len(profile.Models)-1].ID)
					}
					if overrides == "both" || overrides == "effort" {
						in.Effort = ptr(profile.EffortLevels[len(profile.EffortLevels)-1])
					}
					gateway := kagenttest.New()
					t.Cleanup(gateway.Close)
					for _, agent := range profile.Agents {
						gateway.AddAgent(agent.Namespace, agent.Name)
					}
					gateway.AddAgent("kagent", "agent")
					client, err := kagent.New(kagent.Config{GatewayURL: gateway.URL, UserID: "mainloop"})
					if err != nil {
						t.Fatal(err)
					}
					r.ses.Kagent, r.ses.Providers = client, r.svc.Providers.(*providers.Registry)
					r.svc.Workspaces = &overrideWorkspaces{fakeWorkspaces: r.ws, client: client}
					runner := reconcile.NewRunner(r.pool, r.svc.Log, nil)
					loop := r.ses.DeliveryLoop()
					loop.Interval = 10 * time.Millisecond
					runner.Add(loop)
					ctx, cancel := context.WithCancel(context.Background())
					done := make(chan struct{})
					go func() { defer close(done); _ = runner.Run(ctx) }()
					t.Cleanup(func() { cancel(); <-done })
					r.run()
					id := r.mustCreate(r.own, in)
					r.eventually("task provisioned", func() bool { return r.opState(id) == "completed" })
					attemptID, bindingID := r.attemptOf(id)
					attempt, err := dbq.New(r.pool).GetTaskAttempt(ctx, attemptID)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(attempt.Model, in.Model) || !reflect.DeepEqual(attempt.Effort, in.Effort) {
						t.Fatalf("attempt overrides = %v/%v, want %v/%v", attempt.Model, attempt.Effort, in.Model, in.Effort)
					}
					var model, effort *string
					var touched bool
					if err := r.pool.QueryRow(ctx, `SELECT model_override, effort_override, model_effort_touched FROM agent_bindings WHERE session_id = $1`, bindingID).Scan(&model, &effort, &touched); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(model, in.Model) || !reflect.DeepEqual(effort, in.Effort) || touched != (overrides != "nil") {
						t.Fatalf("binding overrides = %v/%v touched=%v", model, effort, touched)
					}
					messageID := tasks.BriefMessageID(attemptID)
					r.eventually("first native turn completed", func() bool {
						return r.count(`SELECT count(*) FROM deliveries WHERE id = $1 AND state = 'completed'`, messageID) == 1
					})
					want := map[string]any{}
					if profile.NativeProvider == "codex" && overrides != "nil" {
						want[kagent.ModelMetadataKey], want[kagent.EffortMetadataKey] = profile.DefaultModel, profile.DefaultEffort
					}
					if in.Model != nil {
						want[kagent.ModelMetadataKey] = *in.Model
					}
					if in.Effort != nil {
						want[kagent.EffortMetadataKey] = *in.Effort
					}
					var sends int
					for _, request := range gateway.Requests() {
						if request.Key != "SendStreamingMessage" || request.MessageID != messageID {
							continue
						}
						sends++
						if len(request.Metadata) != len(want) {
							t.Fatalf("first turn metadata = %v, want %v", request.Metadata, want)
						}
						for key, value := range want {
							if request.Metadata[key] != value {
								t.Errorf("first turn metadata[%s] = %v, want %v", key, request.Metadata[key], value)
							}
						}
					}
					if sends != 1 {
						t.Fatalf("brief native sends = %d, want 1", sends)
					}
				})
			}
		}
	}
}

func TestTaskModelEffortIdempotency(t *testing.T) {
	for _, changed := range []string{"model", "effort", "omitted"} {
		t.Run(changed, func(t *testing.T) {
			r := newRig(t)
			profile := overrideProfile(t, r, "codex")
			in := tasks.CreateInput{RequestID: "model-effort-replay", ProviderProfileID: profile.ID, Model: ptr(profile.Models[0].ID), Effort: ptr(profile.EffortLevels[0])}
			first, err := r.create(r.own, in)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := r.create(r.own, in)
			if err != nil || replay.Id != first.Id {
				t.Fatalf("equal replay = %+v, %v; first %+v", replay, err, first)
			}
			switch changed {
			case "model":
				in.Model = ptr(profile.Models[1].ID)
			case "effort":
				in.Effort = ptr(profile.EffortLevels[1])
			case "omitted":
				in.Model, in.Effort = nil, nil
			}
			_, err = r.create(r.own, in)
			wantRefusal(t, err, 409, tasks.ReasonPayloadConflict)
			if r.count(`SELECT count(*) FROM tasks`) != 1 || r.count(`SELECT count(*) FROM task_operations`) != 1 {
				t.Fatal("replay created additional task or operation rows")
			}
		})
	}
}

func TestRESTTaskModelEffortValidation(t *testing.T) {
	registry, err := providers.NewRegistry(mustConfig(t).Kagent)
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range registry.Profiles() {
		for _, field := range []string{"model", "effort"} {
			t.Run(profile.ID+"/"+field, func(t *testing.T) {
				r := newRest(t)
				body := map[string]any{"request_id": "invalid-override", "title": "Plan", "brief": "Brief", "mode": "coordination", "provider_profile_id": profile.ID, field: "not-an-offered-value"}
				response := r.do("POST", "/tasks", body, 422)
				detail, ok := response["detail"].([]any)
				if !ok || len(detail) != 1 {
					t.Fatalf("validation detail = %#v", response["detail"])
				}
				validation, ok := detail[0].(map[string]any)
				if !ok || validation["type"] != "value_error" {
					t.Fatalf("validation error = %#v", detail[0])
				}
				message, ok := validation["msg"].(string)
				if !ok || !strings.Contains(message, profile.ID) || !strings.Contains(message, "not-an-offered-value") {
					t.Fatalf("validation message = %#v", validation["msg"])
				}
				var allowed []string
				if field == "model" {
					for _, model := range profile.Models {
						allowed = append(allowed, model.ID)
					}
				} else {
					allowed = profile.EffortLevels
				}
				if len(allowed) == 0 {
					t.Fatal("test profile has no allowed values")
				}
				for _, value := range allowed {
					if !strings.Contains(message, value) {
						t.Errorf("message %q omits allowed %s %q", message, field, value)
					}
				}
				if got := r.do("GET", "/tasks", nil, 200)["list"]; got != "[]\n" && got != "[]" {
					t.Fatalf("invalid REST create left tasks: %v", got)
				}
			})
		}
	}
}

func TestTaskModelEffortPersistedSourceFixture(t *testing.T) {
	for _, replacement := range []string{"none", "model", "effort"} {
		t.Run(replacement, func(t *testing.T) {
			r := newRig(t)
			profile := overrideProfile(t, r, "claude")
			sourceModel, sourceEffort := profile.Models[0].ID, profile.EffortLevels[0]
			in := tasks.CreateInput{ProviderProfileID: profile.ID, Mode: dbstate.TaskModeCoordination}
			wantModel, wantEffort := sourceModel, sourceEffort
			if replacement == "model" {
				wantModel = profile.Models[1].ID
				in.Model = &wantModel
			}
			if replacement == "effort" {
				wantEffort = profile.EffortLevels[1]
				in.Effort = &wantEffort
			}
			id := r.mustCreate(r.own, in)
			source := r.attempt(id, dbstate.AttemptSupervisor, dbstate.AttemptCompleted, "")
			r.exec(`UPDATE task_attempts SET model = $2, effort = $3 WHERE id = $1`, source, sourceModel, sourceEffort)
			r.exec(`UPDATE tasks SET current_attempt_id = NULL WHERE id = $1`, id)
			r.exec(`UPDATE task_operations SET source_attempt_id = $2 WHERE task_id = $1 AND kind = 'create'`, id, source)
			r.run()
			r.eventually("fixture successor provisioned", func() bool { return r.opState(id) == "completed" })
			successorID, bindingID := r.attemptOf(id)
			successor, err := dbq.New(r.pool).GetTaskAttempt(context.Background(), successorID)
			if err != nil {
				t.Fatal(err)
			}
			if successorID == source || successor.Number != 2 || successor.Model == nil || *successor.Model != wantModel || successor.Effort == nil || *successor.Effort != wantEffort {
				t.Fatalf("fixture successor = %+v, want model %q effort %q", successor, wantModel, wantEffort)
			}
			if r.str(`SELECT model_override FROM agent_bindings WHERE session_id = $1`, bindingID) != wantModel || r.str(`SELECT effort_override FROM agent_bindings WHERE session_id = $1`, bindingID) != wantEffort {
				t.Fatal("fixture successor binding did not inherit effective overrides")
			}
			if r.str(`SELECT model FROM task_attempts WHERE id = $1`, source) != sourceModel || r.str(`SELECT effort FROM task_attempts WHERE id = $1`, source) != sourceEffort {
				t.Fatal("fixture source overrides changed")
			}
		})
	}
}
