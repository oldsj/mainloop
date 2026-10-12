package health_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/health"
)

// toolCalls builds the started and finished events of tool calls in order. A call is
// "action:path[:outcome]"; the outcome defaults to success.
func toolCalls(calls ...string) []health.Event {
	var events []health.Event
	seq := int64(1)
	for i, c := range calls {
		parts := strings.Split(c, ":")
		var path *string
		if parts[1] != "" {
			path = &parts[1]
		}
		outcome := "success"
		if len(parts) > 2 {
			outcome = parts[2]
		}
		key := fmt.Sprintf("call-%d", i)
		for _, phase := range []string{"started", "finished"} {
			events = append(events, health.Event{Sequence: seq, Tool: &health.Tool{
				CallKey: key, Phase: phase, ToolClass: "builtin", OperationKey: "op", Action: parts[0], Path: path, Outcome: outcome,
			}})
			seq++
		}
	}
	return events
}

func TestSummarizeReorientation(t *testing.T) {
	t.Run("counts reads, repeats and reads before the first edit", func(t *testing.T) {
		got := health.Summarize(toolCalls("read:a.go", "read:b.go", "read:a.go", "search:", "edit:a.go", "read:a.go", "edit:b.go"), nil).Reorientation
		if got.Reads != 4 || got.RereadCalls != 2 || got.Edits != 2 || got.RereadsFromEarlierTurns != 0 ||
			got.ReadsBeforeFirstEdit == nil || *got.ReadsBeforeFirstEdit != 3 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a failed call is not counted and no edit leaves the marker null", func(t *testing.T) {
		got := health.Summarize(toolCalls("read:a.go:error", "read:b.go", "edit:b.go:denied"), nil).Reorientation
		if got.Reads != 1 || got.Edits != 0 || got.RereadCalls != 0 || got.ReadsBeforeFirstEdit != nil {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a path an earlier turn touched counts once, then repeats count as repeats", func(t *testing.T) {
		got := health.Summarize(toolCalls("read:a.go", "read:a.go", "read:c.go"), map[string]bool{"a.go": true}).Reorientation
		if got.Reads != 3 || got.RereadsFromEarlierTurns != 1 || got.RereadCalls != 1 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a read with no safe path counts as a read only", func(t *testing.T) {
		got := health.Summarize(toolCalls("read:", "read:"), map[string]bool{"": true}).Reorientation
		if got.Reads != 2 || got.RereadCalls != 0 || got.RereadsFromEarlierTurns != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a call still running counts", func(t *testing.T) {
		events := toolCalls("read:a.go", "read:a.go")
		got := health.Summarize(events[:3], nil).Reorientation // the second call has started and not finished
		if got.Reads != 2 || got.RereadCalls != 1 {
			t.Fatalf("%+v", got)
		}
	})
}

// toolTask is a kagent task whose observations are one turn of tool calls on one native context,
// dated at.
func toolTask(taskID, epoch, context string, at time.Time, calls ...string) *a2a.Task {
	var arts []*a2a.Artifact
	add := func(seq int, kind string, body map[string]any) {
		id := fmt.Sprintf("%s:%d", epoch, seq)
		data := map[string]any{
			"schema_version": health.Schema, "event_id": id, "producer_epoch": epoch, "sequence": float64(seq), "kind": kind,
			"occurred_at": at.Add(time.Duration(seq) * time.Millisecond).UTC().Format(time.RFC3339Nano), "time_source": "adapter_observed",
			"time_precision": "nanosecond", "provider": "claude", "runtime_version": "2.1.260", "source": "claude_result_stream",
			"turn_key": epoch, "runtime_session_id": context, "a2a_task_id": taskID,
		}
		for k, v := range body {
			data[k] = v
		}
		arts = append(arts, &a2a.Artifact{ID: a2a.ArtifactID(id), Name: health.Schema, Parts: a2a.ContentParts{a2a.NewDataPart(data)}})
	}
	seq := 1
	add(seq, "turn", map[string]any{"turn": map[string]any{"state": "started", "coverage": "partial"}})
	for i, c := range calls {
		parts := strings.Split(c, ":")
		var path any
		if parts[1] != "" {
			path = parts[1]
		}
		for _, phase := range []string{"started", "finished"} {
			seq++
			add(seq, "tool", map[string]any{"tool": map[string]any{
				"call_key": fmt.Sprintf("%s-%d", epoch, i), "phase": phase, "tool_class": "builtin", "operation_key": "op",
				"action": parts[0], "path": path, "outcome": "success", "category": "none",
			}})
		}
	}
	seq++
	add(seq, "turn", map[string]any{"turn": map[string]any{"state": "finished", "coverage": "complete"}})
	return &a2a.Task{ID: a2a.TaskID(taskID), ContextID: context, Artifacts: arts}
}

const (
	epoch1       = "33333333-3333-4333-8333-333333333333"
	epoch2       = "44444444-4444-4444-8444-444444444444"
	otherContext = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

func turnsByTask(t *testing.T, v api.HealthView) map[string]api.HealthTurnSummary {
	t.Helper()
	if len(v.Rows) != 1 {
		t.Fatalf("rows %+v", v.Rows)
	}
	out := map[string]api.HealthTurnSummary{}
	for _, turn := range v.Rows[0].Turns {
		out[turn.A2aTaskId] = turn.Summary
	}
	return out
}

func inContext(context string) func(*health.Input) {
	return func(in *health.Input) { in.ContextID, in.RuntimeSessionID = context, context }
}

// A turn's summary names the paths it re-reads from the turns before it in the same native
// context, and the view adds them up.
func TestIngestCountsRereadsFromEarlierTurns(t *testing.T) {
	e := newEnv(t)
	e.ingest(t, toolTask(task1, epoch1, runtimeSession, e.at, "read:a.go", "read:b.go", "edit:c.go"), nil)
	e.ingest(t, toolTask(task2, epoch2, runtimeSession, e.at.Add(time.Minute), "read:a.go", "read:c.go", "read:d.go", "edit:d.go"), nil)

	v := e.view(t, health.Filter{})
	turns := turnsByTask(t, v)
	r1, r2 := turns[task1].Reorientation, turns[task2].Reorientation
	if r1 == nil || r1.Turns != 1 || r1.EditedTurns != 1 || r1.Reads != 2 || r1.RereadsFromEarlierTurns != 0 || r1.Edits != 1 ||
		r1.ReadsBeforeFirstEdit == nil || *r1.ReadsBeforeFirstEdit != 2 {
		t.Fatalf("first turn %+v", r1)
	}
	if r2 == nil || r2.Reads != 3 || r2.RereadsFromEarlierTurns != 2 || r2.ReadsBeforeFirstEdit == nil || *r2.ReadsBeforeFirstEdit != 3 {
		t.Fatalf("second turn %+v", r2)
	}
	tot := v.Rows[0].Totals.Reorientation
	if tot == nil || tot.Turns != 2 || tot.EditedTurns != 2 || tot.Reads != 5 || tot.RereadsFromEarlierTurns != 2 || tot.Edits != 2 ||
		tot.ReadsBeforeFirstEdit == nil || *tot.ReadsBeforeFirstEdit != 5 {
		t.Fatalf("totals %+v", tot)
	}
}

// A new native context has not seen what the old one read, so reading it again is not a repeat.
func TestRereadsDoNotCrossNativeContexts(t *testing.T) {
	e := newEnv(t)
	e.ingest(t, toolTask(task1, epoch1, runtimeSession, e.at, "read:a.go", "edit:a.go"), nil)
	e.ingest(t, toolTask(task2, epoch2, otherContext, e.at.Add(time.Minute), "read:a.go"), inContext(otherContext))

	r := turnsByTask(t, e.view(t, health.Filter{}))[task2].Reorientation
	if r == nil || r.Reads != 1 || r.RereadsFromEarlierTurns != 0 {
		t.Fatalf("%+v", r)
	}
}

// The earlier-turn count only looks back: a turn that arrives first is not changed by one that
// happened before it but was collected after.
func TestRereadsOnlyLookBack(t *testing.T) {
	e := newEnv(t)
	e.ingest(t, toolTask(task2, epoch2, runtimeSession, e.at.Add(time.Minute), "read:a.go"), nil)
	e.ingest(t, toolTask(task1, epoch1, runtimeSession, e.at, "read:a.go"), nil)

	turns := turnsByTask(t, e.view(t, health.Filter{}))
	// The later turn was summarised before the earlier one existed, so it has nothing to count;
	// the earlier turn must not count the later one.
	if r := turns[task1].Reorientation; r == nil || r.RereadsFromEarlierTurns != 0 {
		t.Fatalf("earlier turn %+v", r)
	}
	if r := turns[task2].Reorientation; r == nil || r.RereadsFromEarlierTurns != 0 {
		t.Fatalf("later turn %+v", r)
	}
}

// A total adds the turns that carry the block, counts them, and averages over the edited ones.
func TestTotalsCountTurnsWithAndWithoutTheBlock(t *testing.T) {
	e := newEnv(t)
	e.ingest(t, toolTask(task1, epoch1, runtimeSession, e.at, "read:a.go", "read:b.go", "edit:a.go"), nil)
	e.ingest(t, toolTask(task2, epoch2, runtimeSession, e.at.Add(time.Minute), "read:c.go"), nil)
	// A turn summarised before the block existed.
	e.exec(t, `UPDATE health_turns SET summary = summary - 'reorientation' WHERE a2a_task_id = $1`, task2)

	v := e.view(t, health.Filter{})
	if v.Rows[0].Totals.Turns != 2 {
		t.Fatalf("turns %d", v.Rows[0].Totals.Turns)
	}
	if r := turnsByTask(t, v)[task2].Reorientation; r != nil {
		t.Fatalf("old turn has %+v", r)
	}
	tot := v.Rows[0].Totals.Reorientation
	if tot == nil || tot.Turns != 1 || tot.EditedTurns != 1 || tot.Reads != 2 || tot.Edits != 1 ||
		tot.ReadsBeforeFirstEdit == nil || *tot.ReadsBeforeFirstEdit != 2 {
		t.Fatalf("totals %+v", tot)
	}
}

// A total of turns that made no edit has no reads-before-first-edit.
func TestTotalsWithoutEdits(t *testing.T) {
	e := newEnv(t)
	e.ingest(t, toolTask(task1, epoch1, runtimeSession, e.at, "read:a.go"), nil)
	tot := e.view(t, health.Filter{}).Rows[0].Totals.Reorientation
	if tot == nil || tot.Turns != 1 || tot.EditedTurns != 0 || tot.ReadsBeforeFirstEdit != nil {
		t.Fatalf("totals %+v", tot)
	}
}
