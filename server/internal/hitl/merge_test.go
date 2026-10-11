package hitl_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/hitl"
	"github.com/oldsj/mainloop/server/internal/kagent"
)

type templateRuntime struct {
	template string
	ref      kagent.AgentRef
	err      error
}

func (r *templateRuntime) SessionAgent(context.Context, string) (kagent.AgentRef, error) {
	return r.ref, r.err
}
func (r *templateRuntime) AgentTemplate(context.Context, kagent.AgentRef) (string, error) {
	return r.template, r.err
}

var mergeConfig = config.MergeConfiguration{TemplateName: "trusted-template", Provider: config.NativeClaude, CompiledAlias: "merge", Tool: config.MergeToolName, RequireApproval: true}

func mergeEnv(t *testing.T, edits ...func(*api.MergeSummary)) (*env, *templateRuntime) {
	t.Helper()
	runtime := &templateRuntime{template: mergeConfig.TemplateName, ref: agent}
	e := newEnv(t, func(s *hitl.Service) {
		s.MergeConfigs = []config.MergeConfiguration{mergeConfig}
		s.MergeRuntime = runtime
	})
	mergeExec(t, e, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('p1', $1, 'owner', 'repo', 'owner/repo', 'https://github.com/owner/repo')`, owner)
	mergeExec(t, e, `UPDATE sessions SET project_id = 'p1' WHERE id = $1`, session)
	title, repository, head, base := "<script>proposal</script>", "owner/repo", "feature", "main"
	sha, baseSHA, dg, pr, policy := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 64), 7, api.MergeSummaryPolicyApproval
	summary := api.MergeSummary{Version: 1, ProposalId: "proposal", Title: &title, Repository: &repository,
		PrNumber: &pr, Head: &head, HeadSha: &sha, Base: &base, BaseSha: &baseSHA,
		Availability: api.MergeSummaryAvailabilityReady, Policy: &policy, PathsPreview: []string{}, ProtectedMatches: []string{}, ApprovalReasons: []api.MergeApprovalReason{}, UnavailableReasons: []string{}}
	for _, edit := range edits {
		edit(&summary)
	}
	presentation, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	mergeExec(t, e, `INSERT INTO merge_candidates (id, idempotency_key, owner_id, project_id, repository_id, repository, pr_number, head_sha, route)
        VALUES ('candidate', 'candidate', $1, 'p1', 1, 'owner/repo', 7, $2, 'approval')`, owner, sha)
	mergeExec(t, e, `INSERT INTO merge_proposals (id, candidate_id, owner_id, session_id, facts, presentation, summary_digest)
        VALUES ('proposal', 'candidate', $1, $2, '{}', $3, $4)`, owner, session, presentation, dg)
	return e, runtime
}

func mergeExec(t *testing.T, e *env, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func mergeAsk() map[string]any {
	return map[string]any{"type": "tool_approval_request", "tools": []any{map[string]any{
		"id": "merge-tool", "call_id": "merge-call", "name": mergeConfig.PublicName(),
		"args": map[string]any{"proposal_id": "proposal", "request_id": "merge-invocation"},
	}}}
}

func TestHITLMergeViewSerializationAndTemplateBinding(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*env, *templateRuntime)
		want   bool
	}{
		{"trusted template", func(*env, *templateRuntime) {}, true},
		{"different template", func(_ *env, r *templateRuntime) { r.template = "other" }, false},
		{"inline template", func(_ *env, r *templateRuntime) { r.template = "" }, false},
		{"runtime unreadable", func(_ *env, r *templateRuntime) { r.err = errors.New("offline") }, false},
		{"different runtime agent", func(_ *env, r *templateRuntime) { r.ref.Name = "other" }, false},
		{"ambiguous mapping", func(e *env, _ *templateRuntime) { e.svc.MergeConfigs = append(e.svc.MergeConfigs, mergeConfig) }, false},
		{"provider mismatch", func(e *env, _ *templateRuntime) {
			c := mergeConfig
			c.Provider = config.NativeCodex
			e.svc.MergeConfigs = append(e.svc.MergeConfigs, c)
			mergeExec(t, e, `UPDATE agent_bindings SET kind = 'codex'`)
		}, false},
		{"project mismatch", func(e *env, _ *templateRuntime) { mergeExec(t, e, `UPDATE sessions SET project_id = NULL`) }, false},
		{"runtime replaced", func(e *env, _ *templateRuntime) {
			mergeExec(t, e, `UPDATE agent_bindings SET kagent_session_id = 'other'`)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, runtime := mergeEnv(t)
			e.k.put(waitingTask("merge-task", "merge-msg", mergeAsk()))
			e.observe()
			request := e.requestFor("merge-task")
			tc.mutate(e, runtime)
			view, err := e.svc.View(context.Background(), request.ID)
			if err != nil {
				t.Fatal(err)
			}
			if view.Merge == nil || len(*view.Merge) != 1 {
				t.Fatalf("merge context: %+v", view.Merge)
			}
			c := (*view.Merge)[0]
			if (c.ProposalId != nil) != tc.want {
				t.Fatalf("association: %+v", c)
			}
			if !tc.want && c.Availability != api.VerifiedMergeContextAvailabilityUnavailable {
				t.Fatalf("untrusted context: %+v", c)
			}
			if !tc.want && (c.PrUrl != nil || c.CompareUrl != nil) {
				t.Fatalf("untrusted context has review links: %+v", c)
			}
			if tc.want {
				raw, err := json.Marshal(view)
				if err != nil {
					t.Fatal(err)
				}
				var wire map[string]any
				if err := json.Unmarshal(raw, &wire); err != nil {
					t.Fatal(err)
				}
				merge := wire["merge"].([]any)[0].(map[string]any)
				if merge["proposal_id"] != "proposal" || merge["repository"] != "owner/repo" || merge["pr_number"] != float64(7) || merge["head_sha"] != strings.Repeat("a", 40) || merge["base"] != "main" || merge["state"] != "prepared" {
					t.Fatalf("wire: %s", raw)
				}
				if merge["summary"].(map[string]any)["title"] != "<script>proposal</script>" {
					t.Fatalf("summary: %s", raw)
				}
				if merge["pr_url"] != "https://github.com/owner/repo/pull/7" || merge["compare_url"] != "https://github.com/owner/repo/compare/main...feature" {
					t.Fatalf("review links: %s", raw)
				}
			}
		})
	}
}

func TestReviewA1TrustedMergeHasReviewLinks(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		value       any
		wantPR      bool
		wantCompare string
	}{
		{"ready", "head", "feature", true, "main...feature"},
		{"encoded head", "head", "feature/retry#one%two", true, "main...feature%2Fretry%23one%25two"},
		{"encoded base", "base", "release/2026", true, "release%2F2026...feature"},
		{"reserved ref characters", "head", "feature/retry+build@one=two&three$four", true, "main...feature%2Fretry%2Bbuild%40one%3Dtwo%26three%24four"},
		{"missing repository", "repository", nil, false, ""},
		{"repository URL", "repository", "https://github.com/owner/repo", false, ""},
		{"repository query", "repository", "owner/repo?tab=files", false, ""},
		{"extra repository segment", "repository", "owner/repo/other", false, ""},
		{"repository traversal", "repository", "owner/..", false, ""},
		{"repository whitespace", "repository", " owner/repo ", false, ""},
		{"missing number", "pr_number", nil, false, "main...feature"},
		{"zero number", "pr_number", 0, false, "main...feature"},
		{"negative number", "pr_number", -1, false, "main...feature"},
		{"missing head", "head", nil, true, ""},
		{"empty head", "head", "", true, ""},
		{"invalid head", "head", "feature...other", true, ""},
		{"head query", "head", "feature?tab=files", true, ""},
		{"missing base", "base", nil, true, ""},
		{"invalid base", "base", "release/../main", true, ""},
		{"base control character", "base", "main\n", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patch, err := json.Marshal(map[string]any{tc.field: tc.value})
			if err != nil {
				t.Fatal(err)
			}
			e, _ := mergeEnv(t, func(summary *api.MergeSummary) {
				if err := json.Unmarshal(patch, summary); err != nil {
					t.Fatal(err)
				}
			})
			e.k.put(waitingTask("merge-task", "merge-msg", mergeAsk()))
			e.observe()
			view, err := e.svc.View(context.Background(), e.requestFor("merge-task").ID)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Merge []map[string]any `json:"merge"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil || len(wire.Merge) != 1 {
				t.Fatalf("wire: %s: %v", raw, err)
			}
			links := wire.Merge[0]
			prURL, hasPR := links["pr_url"]
			compareURL, hasCompare := links["compare_url"]
			if hasPR != tc.wantPR || (hasPR && prURL != "https://github.com/owner/repo/pull/7") {
				t.Fatalf("PR link: %s", raw)
			}
			if hasCompare != (tc.wantCompare != "") || (hasCompare && compareURL != "https://github.com/owner/repo/compare/"+tc.wantCompare) {
				t.Fatalf("compare link: %s", raw)
			}
		})
	}
}

func TestReviewA1EndedProposalKeepsUnansweredNativeInput(t *testing.T) {
	for _, ending := range []string{"expired", "superseded", "blocked"} {
		t.Run(ending, func(t *testing.T) {
			e, _ := mergeEnv(t)
			ctx := context.Background()
			e.k.put(waitingTask("merge-task", "merge-msg", mergeAsk()))
			e.observe()
			r := e.requestFor("merge-task")
			svc := &attention.Service{Pool: e.pool, OwnerID: owner}
			if err := svc.SyncTasks(ctx); err != nil {
				t.Fatal(err)
			}
			snap, err := svc.Snapshot(ctx)
			if err != nil || snap.Count != 1 || snap.Items[0].Id != "merge:proposal" {
				t.Fatalf("proposal attention: %+v: %v", snap, err)
			}
			mergeExec(t, e, `UPDATE merge_candidates SET state = $1`, ending)
			for range 3 {
				if err := svc.SyncTasks(ctx); err != nil {
					t.Fatal(err)
				}
				snap, err = svc.Snapshot(ctx)
				if err != nil || snap.Count != 1 || snap.Items[0].Id != "hitl:"+r.ID || snap.Items[0].QueueItem == nil || *snap.Items[0].QueueItem.HitlRequestId != r.ID {
					t.Fatalf("unanswered native input hidden after %s: %+v: %v", ending, snap, err)
				}
			}
			view, err := e.svc.View(ctx, r.ID)
			if err != nil || !view.Answerable || view.Request.Availability != api.HITLProjectionAvailabilityPending || view.Response != nil {
				t.Fatalf("native request no longer answerable: %+v: %v", view, err)
			}
			var generic, proposal string
			if err := e.pool.QueryRow(ctx, `SELECT
                (SELECT state FROM attention_items WHERE source_kind = 'hitl_request'),
                (SELECT state FROM attention_items WHERE source_kind = 'merge_proposal')`).Scan(&generic, &proposal); err != nil || generic != "open" || proposal != "resolved" {
				t.Fatalf("attention states: generic %s, proposal %s: %v", generic, proposal, err)
			}
			in := decision("reject-ended-proposal", api.ToolApprovalResponse{Type: api.ToolApprovalResponseTypeToolApprovalResponse, Approvals: []api.ToolApproval{{Id: "merge-tool", Approved: false}}})
			if _, err := e.svc.Respond(ctx, r.ID, in); err != nil {
				t.Fatal(err)
			}
			snap, err = svc.Snapshot(ctx)
			if err != nil || snap.Count != 0 {
				t.Fatalf("recorded native response still prompts: %+v: %v", snap, err)
			}
			mergeExec(t, e, `UPDATE hitl_responses SET state = 'rejected_transport'`)
			if err := svc.SyncTasks(ctx); err != nil {
				t.Fatal(err)
			}
			snap, err = svc.Snapshot(ctx)
			if err != nil || snap.Count != 1 || snap.Items[0].Id != "hitl:"+r.ID {
				t.Fatalf("native response rejection did not restore attention: %+v: %v", snap, err)
			}
			mergeExec(t, e, `UPDATE sessions SET status = 'completed'`)
			if err := svc.SyncTasks(ctx); err != nil {
				t.Fatal(err)
			}
			snap, err = svc.Snapshot(ctx)
			if err != nil || snap.Count != 0 {
				t.Fatalf("final session still prompts: %+v: %v", snap, err)
			}
		})
	}
}

func TestMergeProposalAttentionLifecycle(t *testing.T) {
	for _, ending := range []string{"approved", "refused", "superseded", "expired", "blocked", "merged", "deadline", "request unavailable"} {
		t.Run(ending, func(t *testing.T) {
			e, _ := mergeEnv(t)
			ctx := context.Background()
			e.k.put(waitingTask("merge-task", "merge-msg", mergeAsk()))
			e.observe()
			r := e.requestFor("merge-task")
			svc := &attention.Service{Pool: e.pool, OwnerID: owner}
			sweep := func(wantID string) {
				t.Helper()
				want := 0
				if wantID != "" {
					want = 1
				}
				if err := svc.SyncTasks(ctx); err != nil {
					t.Fatal(err)
				}
				snap, err := svc.Snapshot(ctx)
				if err != nil || snap.Count != want {
					t.Fatalf("snapshot %+v %v", snap, err)
				}
				if want == 1 && (snap.Items[0].Id != wantID || snap.Items[0].QueueItem == nil || *snap.Items[0].QueueItem.HitlRequestId != r.ID) {
					t.Fatalf("item %+v", snap.Items[0])
				}
				if wantID == "merge:proposal" && (snap.Items[0].ReasonCode == nil || *snap.Items[0].ReasonCode != api.AttentionItemReasonCodeMergeApproval) {
					t.Fatalf("merge reason: %+v", snap.Items[0])
				}
			}
			unchanged := func(wantID string) {
				t.Helper()
				var before, after time.Time
				var eventsBefore, eventsAfter int
				query := `SELECT updated_at, (SELECT count(*) FROM events WHERE type = 'inbox:updated') FROM attention_items WHERE source_kind = 'merge_proposal'`
				if err := e.pool.QueryRow(ctx, query).Scan(&before, &eventsBefore); err != nil {
					t.Fatal(err)
				}
				for range 3 {
					sweep(wantID)
				}
				if err := e.pool.QueryRow(ctx, query).Scan(&after, &eventsAfter); err != nil {
					t.Fatal(err)
				}
				if !before.Equal(after) || eventsBefore != eventsAfter {
					t.Fatal("unchanged merge attention churned")
				}
			}
			sweep("merge:proposal")
			unchanged("merge:proposal")
			switch ending {
			case "approved", "refused":
				in := decision("decision", api.ToolApprovalResponse{Type: api.ToolApprovalResponseTypeToolApprovalResponse, Approvals: []api.ToolApproval{{Id: "merge-tool", Approved: ending == "approved"}}})
				if _, err := e.svc.Respond(ctx, r.ID, in); err != nil {
					t.Fatal(err)
				}
			case "deadline":
				mergeExec(t, e, `UPDATE merge_candidates SET deadline = now() - interval '1 second'`)
			case "request unavailable":
				e.k.set(func() { e.k.tasks["merge-task"].Status.State = a2a.TaskStateWorking })
				e.observe()
				view, err := e.svc.View(ctx, r.ID)
				if err != nil || view.Request.Availability != api.HITLProjectionAvailabilityUnavailable {
					t.Fatalf("native request availability: %+v: %v", view, err)
				}
			case "merged":
				mergeExec(t, e, `UPDATE merge_candidates SET state = 'merged', merge_sha = repeat('d', 40)`)
			default:
				mergeExec(t, e, `UPDATE merge_candidates SET state = $1`, ending)
			}
			wantID := "hitl:" + r.ID
			if ending == "approved" || ending == "refused" || ending == "request unavailable" {
				wantID = ""
			}
			sweep(wantID)
			unchanged(wantID)
			var state string
			if err := e.pool.QueryRow(ctx, `SELECT state FROM attention_items WHERE source_kind = 'merge_proposal'`).Scan(&state); err != nil || state != "resolved" {
				t.Fatalf("state %s: %v", state, err)
			}
			if ending == "approved" || ending == "refused" {
				mergeExec(t, e, `UPDATE hitl_responses SET state = 'rejected_transport'`)
				sweep("merge:proposal")
				unchanged("merge:proposal")
			}
		})
	}
}

func TestNewServiceBindsMergeConfiguration(t *testing.T) {
	cfg := &config.Config{MergeConfigurations: config.MergeConfigurations{mergeConfig}}
	svc := hitl.NewService(cfg, nil, nil, nil, nil, nil)
	if len(svc.MergeConfigs) != 1 || svc.MergeConfigs[0].TemplateName != mergeConfig.TemplateName {
		t.Fatal("configuration was not wired")
	}
}

func TestObserverWithholdsUntrustedMergeBinding(t *testing.T) {
	e, runtime := mergeEnv(t)
	runtime.template = "untrusted-template"
	e.k.put(waitingTask("merge-task", "merge-msg", mergeAsk()))
	e.observe()
	var associations int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM hitl_merge_tools`).Scan(&associations); err != nil || associations != 0 {
		t.Fatalf("associations %d: %v", associations, err)
	}
	svc := &attention.Service{Pool: e.pool, OwnerID: owner}
	if err := svc.SyncTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, err := svc.Snapshot(context.Background())
	if err != nil || snap.Count != 1 || !strings.HasPrefix(snap.Items[0].Id, "hitl:") {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	// A later successful read binds it without another native request or a browser read.
	runtime.template = mergeConfig.TemplateName
	e.observe()
	snap, err = svc.Snapshot(context.Background())
	if err != nil || snap.Count != 1 || snap.Items[0].Id != "merge:proposal" {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
}

func TestCodexTemplateBindsMergeCard(t *testing.T) {
	e, _ := mergeEnv(t)
	c := mergeConfig
	c.Provider = config.NativeCodex
	e.svc.MergeConfigs = []config.MergeConfiguration{c}
	mergeExec(t, e, `UPDATE agent_bindings SET kind = 'codex'`)
	ask := mergeAsk()
	ask["tools"].([]any)[0].(map[string]any)["name"] = c.PublicName()
	e.k.put(waitingTask("merge-task", "merge-msg", ask))
	e.observe()
	view, err := e.svc.View(context.Background(), e.requestFor("merge-task").ID)
	if err != nil || view.Merge == nil || len(*view.Merge) != 1 || (*view.Merge)[0].ProposalId == nil {
		t.Fatalf("view %+v: %v", view, err)
	}
}

func TestMergeAttentionDoesNotChurnForEndedTaskAttempt(t *testing.T) {
	e, _ := mergeEnv(t)
	ctx := context.Background()
	e.k.put(waitingTask("merge-task", "merge-msg", mergeAsk()))
	e.observe()
	mergeExec(t, e, `INSERT INTO tasks (id, owner_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status)
        VALUES ('task', $1, 'task', 'Task', 'brief', 'coordination', 'p', 'explicit', 'running')`, owner)
	mergeExec(t, e, `INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider, configuration_revision, agent_namespace, agent_name, role, depth, state, session_id)
        VALUES ('attempt', 'task', 1, 'p', 'claude', 'r', 'ns', 'a', 'supervisor', 1, 'active', $1)`, session)
	mergeExec(t, e, `UPDATE tasks SET status = 'cancelled', current_attempt_id = 'attempt'`)
	svc := &attention.Service{Pool: e.pool, OwnerID: owner}
	if err := svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	var before, after time.Time
	query := `SELECT updated_at FROM attention_items WHERE source_kind = 'merge_proposal'`
	if err := e.pool.QueryRow(ctx, query).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := svc.SyncTasks(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.pool.QueryRow(ctx, query).Scan(&after); err != nil || !before.Equal(after) {
		t.Fatalf("item churned: %v", err)
	}
	snap, err := svc.Snapshot(ctx)
	if err != nil || snap.Count != 0 {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
}

func TestMixedMergeAttentionPreservesOtherInput(t *testing.T) {
	e, _ := mergeEnv(t)
	ask := mergeAsk()
	ask["tools"] = append(ask["tools"].([]any), map[string]any{"id": "other", "call_id": "other", "name": "Bash", "args": map[string]any{"cmd": "ls"}})
	e.k.put(waitingTask("merge-task", "merge-msg", ask))
	e.observe()
	ctx := context.Background()
	svc := &attention.Service{Pool: e.pool, OwnerID: owner}
	if err := svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := svc.Snapshot(ctx)
	if err != nil || snap.Count != 1 || snap.Items[0].Id != "merge:proposal" {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	mergeExec(t, e, `UPDATE merge_candidates SET state = 'expired'`)
	if err := svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err = svc.Snapshot(ctx)
	if err != nil || snap.Count != 1 || !strings.HasPrefix(snap.Items[0].Id, "hitl:") {
		t.Fatalf("other input hidden: %+v %v", snap, err)
	}
}

func TestMergeAttentionMarksSessionWaitingOnInput(t *testing.T) {
	e, _ := mergeEnv(t)
	mergeExec(t, e, `UPDATE agent_bindings SET role = 'agent'`)
	e.k.put(waitingTask("merge-task", "merge-msg", mergeAsk()))
	e.observe()
	svc := &attention.Service{Pool: e.pool, OwnerID: owner}
	snap, err := svc.Snapshot(context.Background())
	if err != nil || len(snap.Sessions) != 1 || !snap.Sessions[0].NeedsOwner || snap.Sessions[0].Status != api.SessionStatusWaitingOnUser {
		t.Fatalf("session attention %+v: %v", snap, err)
	}
}
