package mcpserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/transcripts"
)

// seedTranscript stores one finished turn for an agent's session, from a kagent Session that is no
// longer followed (so a read makes no kagent call): the prompt, a tool call, and a long tool result.
func (h *harness) seedTranscript(t *testing.T, agent authtest.Seeded, words string) {
	t.Helper()
	ctx := context.Background()
	q := dbq.New(h.env.Pool)
	ks := "ks-" + agent.SessionID
	if err := q.InsertTranscriptSession(ctx, dbq.InsertTranscriptSessionParams{
		KagentSessionID: ks, SessionID: agent.SessionID, OwnerID: agent.UserID, AgentNamespace: "agents", AgentName: "claude",
	}); err != nil {
		t.Fatal(err)
	}
	call := a2a.NewDataPart(map[string]any{"id": "c1", "name": "Bash", "args": map[string]any{"command": "make " + words}})
	call.Metadata = map[string]any{"kagent.dev/a2a/part-type": "function_call"}
	result := a2a.NewDataPart(map[string]any{"id": "c1", "name": "Bash", "response": map[string]any{
		"result": words + " started\n" + strings.Repeat("line of build output\n", 300), "isError": false,
	}})
	result.Metadata = map[string]any{"kagent.dev/a2a/part-type": "function_response"}
	task := &a2a.Task{
		ID: a2a.TaskID("turn-" + agent.SessionID), Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		History: []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("please run "+words))},
		Artifacts: []*a2a.Artifact{
			{ID: "a1", Parts: []*a2a.Part{call}},
			{ID: "a2", Parts: []*a2a.Part{result}},
		},
	}
	if _, err := transcripts.Save(ctx, q, transcripts.Copy{
		KagentSessionID: ks, SessionID: agent.SessionID, TaskID: string(task.ID), TaskState: "completed",
		Entries: transcripts.Normalize(task, transcripts.DefaultLimits),
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.EndTranscriptSession(ctx, dbq.EndTranscriptSessionParams{KagentSessionID: ks, State: "complete"}); err != nil {
		t.Fatal(err)
	}
}

func TestTranscriptToolsFollowTaskAuthority(t *testing.T) {
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
	supTask, _ := h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptActive, sup)
	child := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleChild, Grant: dbstate.GrantWorkspace, Parent: sup.SessionID})
	childTask, childAttempt := h.seedTask(t, &supTask, dbstate.AttemptChild, dbstate.AttemptActive, child)
	other := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleChild, Grant: dbstate.GrantWorkspace, Parent: sup.SessionID})
	otherTask, _ := h.seedTask(t, &supTask, dbstate.AttemptChild, dbstate.AttemptActive, other)
	h.seedTranscript(t, child, "zebrafish")
	h.seedTranscript(t, other, "okapi")

	// The supervisor reads its child's transcript without a turn of the child's.
	_, r := h.call(ordinary, sup.Token, "task_transcript", map[string]any{"task_id": childTask})
	sc := r.Result.StructuredContent
	entries, _ := sc["entries"].([]any)
	if r.Result.IsError || len(entries) != 3 || sc["attempt_id"] != childAttempt || sc["has_more"] != false {
		t.Fatalf("task_transcript = %v %q", sc, r.text())
	}
	res := entries[2].(map[string]any)
	if res["kind"] != "tool_result" || res["cut"] != true || len(res["text"].(string)) > 2<<10+8 || !strings.Contains(r.text(), "zebrafish started") {
		t.Fatalf("result entry = %v", res)
	}

	// entry_id reads that one entry whole.
	id := res["id"].(float64)
	_, full := h.call(ordinary, sup.Token, "task_transcript", map[string]any{"task_id": childTask, "entry_id": id})
	whole := full.Result.StructuredContent["entry"].(map[string]any)
	if full.Result.IsError || whole["cut"] != false || len(whole["text"].(string)) <= 2<<10 {
		t.Fatalf("entry_id = %v %q", full.Result.StructuredContent, full.text())
	}

	// kinds narrows the page.
	_, calls := h.call(ordinary, sup.Token, "task_transcript", map[string]any{"task_id": childTask, "kinds": []string{"tool_call"}})
	if got, _ := calls.Result.StructuredContent["entries"].([]any); len(got) != 1 || got[0].(map[string]any)["tool_name"] != "Bash" {
		t.Fatalf("kinds = %v", calls.Result.StructuredContent)
	}

	// A child reads its own task, and nothing of its sibling's: not the task, not an entry by id
	// through its own task, not a search hit.
	if _, r := h.call(ordinary, child.Token, "task_transcript", map[string]any{"task_id": otherTask}); !r.Result.IsError || r.text() != "[404] task_not_found" {
		t.Fatalf("sibling's task = %v %q", r.Result.IsError, r.text())
	}
	_, theirs := h.call(ordinary, sup.Token, "task_transcript", map[string]any{"task_id": otherTask})
	theirID := theirs.Result.StructuredContent["entries"].([]any)[0].(map[string]any)["id"]
	if _, r := h.call(ordinary, child.Token, "task_transcript", map[string]any{"task_id": childTask, "entry_id": theirID}); !r.Result.IsError || !strings.Contains(r.text(), "404") {
		t.Fatalf("sibling's entry through own task = %v %q", r.Result.IsError, r.text())
	}
	search := func(token, q string) []any {
		t.Helper()
		_, r := h.call(ordinary, token, "transcript_search", map[string]any{"q": q})
		if r.Result.IsError {
			t.Fatalf("search %q: %q", q, r.text())
		}
		hits, _ := r.Result.StructuredContent["hits"].([]any)
		return hits
	}
	if hits := search(child.Token, "okapi"); len(hits) != 0 {
		t.Fatalf("child found its sibling's transcript: %v", hits)
	}
	hits := search(child.Token, "zebrafish")
	if len(hits) == 0 || hits[0].(map[string]any)["task_id"] != childTask {
		t.Fatalf("child's own search = %v", hits)
	}
	if hits := search(sup.Token, "okapi"); len(hits) == 0 || hits[0].(map[string]any)["task_id"] != otherTask {
		t.Fatalf("supervisor's search = %v", hits)
	}
	_, narrowed := h.call(ordinary, main.Token, "transcript_search", map[string]any{"q": "okapi", "task_id": childTask})
	if got, _ := narrowed.Result.StructuredContent["hits"].([]any); len(got) != 0 {
		t.Fatalf("task_id did not narrow: %v", got)
	}
}

func TestTaskTranscriptBudget(t *testing.T) {
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
	supTask, _ := h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptActive, sup)
	// 40 turns, each with a long result shown as a 2 KiB preview: past the 64 KiB result budget.
	ctx := context.Background()
	q := dbq.New(h.env.Pool)
	ks := "ks-budget"
	if err := q.InsertTranscriptSession(ctx, dbq.InsertTranscriptSessionParams{
		KagentSessionID: ks, SessionID: sup.SessionID, OwnerID: sup.UserID, AgentNamespace: "agents", AgentName: "claude",
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		task := &a2a.Task{
			ID: a2a.TaskID("turn-" + string(rune('A'+i))), Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
			Artifacts: []*a2a.Artifact{{ID: "a", Parts: []*a2a.Part{a2a.NewTextPart(strings.Repeat("words and more words ", 200))}}},
		}
		if _, err := transcripts.Save(ctx, q, transcripts.Copy{
			KagentSessionID: ks, SessionID: sup.SessionID, TaskID: string(task.ID), TaskState: "completed",
			Entries: transcripts.Normalize(task, transcripts.DefaultLimits),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.EndTranscriptSession(ctx, dbq.EndTranscriptSessionParams{KagentSessionID: ks, State: "complete"}); err != nil {
		t.Fatal(err)
	}
	_, r := h.call(ordinary, main.Token, "task_transcript", map[string]any{"task_id": supTask})
	sc := r.Result.StructuredContent
	entries, _ := sc["entries"].([]any)
	if r.Result.IsError || sc["has_more"] != true || len(entries) == 0 || len(entries) >= 40 || len(r.text()) > 80<<10 {
		t.Fatalf("budget: %d entries, has_more %v, %d bytes", len(entries), sc["has_more"], len(r.text()))
	}
	last := entries[len(entries)-1].(map[string]any)["id"]
	if sc["next_after"] != last {
		t.Fatalf("next_after %v, last %v", sc["next_after"], last)
	}
	_, next := h.call(ordinary, main.Token, "task_transcript", map[string]any{"task_id": supTask, "after": last})
	if got, _ := next.Result.StructuredContent["entries"].([]any); len(got) == 0 || got[0].(map[string]any)["id"].(float64) <= last.(float64) {
		t.Fatalf("next page = %v", next.Result.StructuredContent)
	}
}
