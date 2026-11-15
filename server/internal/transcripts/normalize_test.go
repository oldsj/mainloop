package transcripts

import (
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Fixtures are fictional: no value below is a real credential, path or session.

var t0 = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func at(ms int) string { return t0.Add(time.Duration(ms) * time.Millisecond).Format(time.RFC3339Nano) }

func withPos[T interface{ SetMeta(string, any) }](v T, ms int) T {
	v.SetMeta(timelinePositionKey, at(ms))
	return v
}

func toolCall(id, name string, args map[string]any) *a2a.Part {
	p := a2a.NewDataPart(map[string]any{"id": id, "name": name, "args": args})
	p.SetMeta(partTypeKey, callKind)
	return p
}

func toolResult(id, name string, result any, isErr bool) *a2a.Part {
	resp := map[string]any{"result": result}
	if isErr {
		resp["isError"] = true
	}
	p := a2a.NewDataPart(map[string]any{"id": id, "name": name, "response": resp})
	p.SetMeta(partTypeKey, resultKind)
	return p
}

func artifact(id string, ms int, parts ...*a2a.Part) *a2a.Artifact {
	return withPos(&a2a.Artifact{ID: a2a.ArtifactID(id), Parts: parts}, ms)
}

// turn is a harness turn as kagent records it: the user message in the history, then (by timeline
// position) text, a tool call and its result, and more text, with the artifacts listed out of order.
func turn() *a2a.Task {
	user := withPos(a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("Find where the retry budget is set.")), 0)
	health := artifact("h1", 15, a2a.NewDataPart(map[string]any{"schema_version": "x"}))
	health.Name = healthPrefix + "v1"
	return &a2a.Task{
		ID: "task-1", ContextID: "ctx-1",
		History: []*a2a.Message{user},
		Status:  a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{
			artifact("a4", 40, a2a.NewTextPart("It is set in retry.go: three attempts.")),
			artifact("a1", 10, a2a.NewTextPart("Let me search.")),
			health,
			artifact("a2", 20, toolCall("call-1", "Grep", map[string]any{"pattern": "retryBudget", "path": "server"})),
			artifact("a3", 30, toolResult("call-1", "Grep", []any{map[string]any{"type": "text", "text": "server/retry.go:12: retryBudget = 3"}}, false)),
		},
	}
}

func TestNormalizeOrdersByTimelineAndKinds(t *testing.T) {
	got := Normalize(turn(), DefaultLimits)
	want := []struct {
		kind Kind
		tool string
		text string
	}{
		{KindUser, "", "Find where the retry budget is set."},
		{KindAssistant, "", "Let me search."},
		{KindToolCall, "Grep", `{"path":"server","pattern":"retryBudget"}`},
		{KindToolResult, "Grep", "server/retry.go:12: retryBudget = 3"},
		{KindAssistant, "", "It is set in retry.go: three attempts."},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries: %+v", len(got), got)
	}
	for i, w := range want {
		e := got[i]
		if e.Pos != i || e.Kind != w.kind || e.ToolName != w.tool || e.Text != w.text {
			t.Errorf("entry %d = %+v, want %+v", i, e, w)
		}
		if e.At == nil {
			t.Errorf("entry %d has no position", i)
		}
	}
	if got[2].ToolCallID != "call-1" || got[3].ToolCallID != "call-1" {
		t.Errorf("call ids %q %q", got[2].ToolCallID, got[3].ToolCallID)
	}
}

func TestNormalizeIsDeterministic(t *testing.T) {
	a, b := Normalize(turn(), DefaultLimits), Normalize(turn(), DefaultLimits)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two reads of one task differ")
	}
}

func TestNormalizeWithoutPositionsKeepsMessagesFirst(t *testing.T) {
	task := &a2a.Task{
		History:   []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))},
		Artifacts: []*a2a.Artifact{{ID: "x", Parts: a2a.ContentParts{a2a.NewTextPart("hello")}}},
		Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
	}
	got := Normalize(task, DefaultLimits)
	if len(got) != 2 || got[0].Kind != KindUser || got[1].Kind != KindAssistant || got[0].At != nil {
		t.Fatalf("%+v", got)
	}
}

func TestNormalizeStatusAskAndError(t *testing.T) {
	ask := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("May I run the migration?"))
	waiting := &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: ask}}
	if got := Normalize(waiting, DefaultLimits); len(got) != 1 || got[0].Kind != KindAsk {
		t.Fatalf("ask: %+v", got)
	}
	failed := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("harness exited"))
	task := &a2a.Task{
		History: []*a2a.Message{failed}, // the status message is also in the history: listed once
		Status:  a2a.TaskStatus{State: a2a.TaskStateFailed, Message: failed},
		Artifacts: []*a2a.Artifact{
			{ID: "r", Parts: a2a.ContentParts{toolResult("c", "Bash", "exit 1", true)}},
		},
	}
	got := Normalize(task, DefaultLimits)
	if len(got) != 2 || got[0].Kind != KindStatus || got[1].Kind != KindToolResult || !got[1].IsError {
		t.Fatalf("failed: %+v", got)
	}
}

func TestNormalizeThoughtDataAndFileParts(t *testing.T) {
	thought := a2a.NewTextPart("weighing two options")
	thought.SetMeta(thoughtKey, true)
	file := a2a.NewFileURLPart("https://files.example/x.png", "image/png")
	file.Filename = "x.png"
	task := &a2a.Task{
		Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{{ID: "a", Parts: a2a.ContentParts{
			a2a.NewTextPart("one "), a2a.NewTextPart("run"), thought, a2a.NewDataPart(map[string]any{"b": 2, "a": 1}), file,
		}}},
	}
	got := Normalize(task, DefaultLimits)
	want := []Entry{
		{Pos: 0, Kind: KindAssistant, Text: "one run", OrigBytes: 7},
		{Pos: 1, Kind: KindThinking, Text: "weighing two options", OrigBytes: 20},
		{Pos: 2, Kind: KindAssistant, Text: `{"a":1,"b":2}`, OrigBytes: 13},
		{Pos: 3, Kind: KindOther, Text: "[file image/png x.png]", OrigBytes: 22},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestNormalizeRedactsBeforeCutting(t *testing.T) {
	key := "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("TUlJRXZRSUJBREFOQmdrcWhraUc5dzBC\n", 40) + "-----END PRIVATE KEY-----"
	// The key straddles where the result is cut: cut first, the tail would keep key lines without
	// the BEGIN line that redaction recognises.
	out := strings.Repeat("before ", 200) + "\n" + key + "\n" + strings.Repeat("after ", 10)
	args := map[string]any{"command": "deploy --token=fictional-token-value-1234"}
	task := &a2a.Task{
		Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{
			{ID: "c", Parts: a2a.ContentParts{toolCall("c1", "Bash", args)}},
			{ID: "r", Parts: a2a.ContentParts{toolResult("c1", "Bash", out, false)}},
		},
	}
	got := Normalize(task, Limits{Text: 1 << 10, Result: 800})
	if strings.Contains(got[0].Text, "fictional-token-value") {
		t.Errorf("call args leak: %q", got[0].Text)
	}
	r := got[1]
	if strings.Contains(r.Text, "TUlJRXZR") || !r.Truncated || r.OrigBytes != len(out) {
		t.Errorf("result: truncated=%v orig=%d %q", r.Truncated, r.OrigBytes, r.Text)
	}
}

func TestNormalizeCaps(t *testing.T) {
	long := strings.Repeat("é", 3000) // two bytes a rune
	task := &a2a.Task{
		Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{
			{ID: "t", Parts: a2a.ContentParts{a2a.NewTextPart(long)}},
			{ID: "r", Parts: a2a.ContentParts{toolResult("c", "Read", "HEAD"+long+"TAIL", false)}},
		},
	}
	got := Normalize(task, Limits{Text: 1001, Result: 1001})
	txt, res := got[0], got[1]
	if !txt.Truncated || len(txt.Text) > 1001 || !utf8.ValidString(txt.Text) || txt.OrigBytes != len(long) {
		t.Errorf("text: %d bytes, truncated=%v valid=%v", len(txt.Text), txt.Truncated, utf8.ValidString(txt.Text))
	}
	if !res.Truncated || len(res.Text) > 1001 || !utf8.ValidString(res.Text) ||
		!strings.HasPrefix(res.Text, "HEAD") || !strings.HasSuffix(res.Text, "TAIL") || !strings.Contains(res.Text, "bytes omitted") {
		t.Errorf("result: %d bytes, %q…", len(res.Text), res.Text[:40])
	}
	short := Normalize(&a2a.Task{Artifacts: []*a2a.Artifact{{ID: "s", Parts: a2a.ContentParts{a2a.NewTextPart("ok")}}}}, DefaultLimits)
	if short[0].Truncated || short[0].Text != "ok" {
		t.Errorf("short: %+v", short[0])
	}
}

func TestNormalizeWithholdsCredentialReads(t *testing.T) {
	task := &a2a.Task{
		Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{
			{ID: "c1", Parts: a2a.ContentParts{toolCall("c1", "Read", map[string]any{"file_path": "/workspace/app/.env"})}},
			{ID: "r1", Parts: a2a.ContentParts{toolResult("c1", "Read", "PLAIN_LOOKING=abc123", false)}},
			{ID: "c2", Parts: a2a.ContentParts{toolCall("c2", "Read", map[string]any{"file_path": "/workspace/app/main.go"})}},
			{ID: "r2", Parts: a2a.ContentParts{toolResult("c2", "Read", "package main", false)}},
		},
	}
	got := Normalize(task, DefaultLimits)
	if r := got[1]; r.Text != Withheld || !r.Truncated || r.OrigBytes != len("PLAIN_LOOKING=abc123") {
		t.Errorf("withheld: %+v", r)
	}
	if got[0].Text != `{"file_path":"/workspace/app/.env"}` {
		t.Errorf("the call itself is kept: %q", got[0].Text)
	}
	if got[3].Text != "package main" {
		t.Errorf("ordinary read: %+v", got[3])
	}
}
