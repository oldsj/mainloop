package health_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

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

// toolTask is a kagent task whose observations are one turn of tool calls, dated at.
func toolTask(taskID, epoch string, at time.Time, calls ...string) *a2a.Task {
	var arts []*a2a.Artifact
	add := func(seq int, kind string, body map[string]any) {
		id := fmt.Sprintf("%s:%d", epoch, seq)
		data := map[string]any{
			"schema_version": health.Schema, "event_id": id, "producer_epoch": epoch, "sequence": float64(seq), "kind": kind,
			"occurred_at": at.Add(time.Duration(seq) * time.Millisecond).UTC().Format(time.RFC3339Nano), "time_source": "adapter_observed",
			"time_precision": "nanosecond", "provider": "claude", "runtime_version": "2.1.260", "source": "claude_result_stream",
			"turn_key": epoch, "runtime_session_id": runtimeSession, "a2a_task_id": taskID,
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
	return &a2a.Task{ID: a2a.TaskID(taskID), ContextID: runtimeSession, Artifacts: arts}
}

// A turn's summary names the paths it re-reads from the turns before it in the session, and the
// view adds them up.
func TestIngestCountsRereadsFromEarlierTurns(t *testing.T) {
	e := newEnv(t)
	first := toolTask(task1, "33333333-3333-4333-8333-333333333333", e.at, "read:a.go", "read:b.go", "edit:c.go")
	second := toolTask(task2, "44444444-4444-4444-8444-444444444444", e.at.Add(time.Minute), "read:a.go", "read:c.go", "read:d.go", "edit:d.go")
	e.ingest(t, first, nil)
	e.ingest(t, second, nil)

	v := e.view(t, health.Filter{})
	if len(v.Rows) != 1 || len(v.Rows[0].Turns) != 2 {
		t.Fatalf("rows %+v", v.Rows)
	}
	byTask := map[string]*int{}
	for _, turn := range v.Rows[0].Turns {
		r := turn.Summary.Reorientation
		if r == nil {
			t.Fatalf("turn %s has no reorientation", turn.A2aTaskId)
		}
		byTask[turn.A2aTaskId] = &r.RereadsFromEarlierTurns
		switch turn.A2aTaskId {
		case task1:
			if r.Reads != 2 || r.RereadsFromEarlierTurns != 0 || r.Edits != 1 || r.ReadsBeforeFirstEdit == nil || *r.ReadsBeforeFirstEdit != 2 {
				t.Fatalf("first turn %+v", r)
			}
		case task2:
			if r.Reads != 3 || r.RereadsFromEarlierTurns != 2 || r.ReadsBeforeFirstEdit == nil || *r.ReadsBeforeFirstEdit != 3 {
				t.Fatalf("second turn %+v", r)
			}
		}
	}
	tot := v.Rows[0].Totals.Reorientation
	if tot == nil || tot.Reads != 5 || tot.RereadsFromEarlierTurns != 2 || tot.Edits != 2 ||
		tot.ReadsBeforeFirstEdit == nil || *tot.ReadsBeforeFirstEdit != 5 {
		t.Fatalf("totals %+v", tot)
	}
}
