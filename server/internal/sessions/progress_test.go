package sessions_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/progress"
)

// liveLog records the live frames the tracker publishes.
type liveLog struct {
	mu     sync.Mutex
	frames []api.EventSessionProgress
}

func (l *liveLog) DispatchLive(_ string, typ events.Type, payload json.RawMessage) {
	if typ != events.TypeSessionProgress {
		return
	}
	var f api.EventSessionProgress
	if err := json.Unmarshal(payload, &f); err != nil {
		panic(err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.frames = append(l.frames, f)
}

func (l *liveLog) all() []api.EventSessionProgress {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]api.EventSessionProgress(nil), l.frames...)
}

func (l *liveLog) waitFor(t *testing.T, what string, ok func(api.EventSessionProgress) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range l.all() {
			if ok(f) {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no frame with %s; frames %+v", what, l.all())
}

func (e *env) withProgress(interval time.Duration) *liveLog {
	log := &liveLog{}
	e.svc.Progress = progress.New(log, progress.WithInterval(interval))
	return log
}

func agentArtifact(id, s string, appendTo bool) *a2a.TaskArtifactUpdateEvent {
	ev := a2a.NewArtifactEvent(&a2a.Task{ID: "t-live", ContextID: "ctx"}, a2a.NewTextPart(s))
	ev.Artifact.ID = a2a.ArtifactID(id)
	ev.Append = appendTo
	return ev
}

func toolArtifact(kind string, data map[string]any) *a2a.TaskArtifactUpdateEvent {
	p := a2a.NewDataPart(data)
	p.Metadata = map[string]any{"kagent.dev/a2a/part-type": kind}
	ev := a2a.NewArtifactEvent(&a2a.Task{ID: "t-live", ContextID: "ctx"}, p)
	ev.Artifact.ID = a2a.ArtifactID(kind + "-" + data["id"].(string))
	return ev
}

func liveTurn() (steps []kagenttest.Step, final *a2a.Task) {
	running := &a2a.Task{ID: "t-live", ContextID: "ctx", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	final = &a2a.Task{
		ID: "t-live", ContextID: "ctx", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart("Hello world")}}},
	}
	steps = []kagenttest.Step{
		kagenttest.Emit{Event: running},
		kagenttest.Emit{Event: agentArtifact("a1", "Hello", false)},
		kagenttest.Emit{Event: agentArtifact("a1", " wor", true)},
		kagenttest.Emit{Event: toolArtifact("function_call", map[string]any{
			"id": "c1", "name": "Bash", "args": map[string]any{"command": "go test ./..."}})},
		kagenttest.Pause{D: 600 * time.Millisecond},
		kagenttest.Emit{Event: toolArtifact("function_response", map[string]any{
			"id": "c1", "name": "Bash", "response": map[string]any{"result": "ok"}})},
		kagenttest.Emit{Event: agentArtifact("a1", "ld", true)},
		kagenttest.Emit{Event: final},
	}
	return steps, final
}

func TestALiveTurnPublishesGrowingTextAndToolLinesThenEndsNamingTheStoredReply(t *testing.T) {
	e := newEnv(t)
	live := e.withProgress(40 * time.Millisecond)
	steps, final := liveTurn()
	e.srv.Script(steps...)
	e.srv.AddTask(final)
	e.run()
	id := e.send("hi")

	// Mid-turn: the partial text, the running tool and the status are out, and nothing is stored yet.
	live.waitFor(t, "the running Bash tool", func(f api.EventSessionProgress) bool {
		return f.Active && f.Status.Label == "Running Bash: go test ./..." && f.Text == "Hello wor" &&
			len(f.Tools) == 1 && f.Tools[0].State == api.SessionProgressToolStateRunning
	})
	if msgs := e.messages(); len(msgs) != 1 {
		t.Fatalf("%d messages mid-turn, want only the user's", len(msgs))
	}

	e.waitState(id, dbstate.DeliveryCompleted)
	live.waitFor(t, "the closing frame", func(f api.EventSessionProgress) bool { return !f.Active })

	msgs := e.messages()
	if len(msgs) != 2 || msgs[1].Content != "Hello world" {
		t.Fatalf("messages %+v", msgs)
	}
	if evs := e.sessionEvents(); len(evs) != 1 {
		t.Fatalf("%d session:message events, want the one the settle wrote", len(evs))
	}

	frames := live.all()
	last := frames[len(frames)-1]
	if last.Active || last.ReplyId == nil || *last.ReplyId != msgs[1].ID || last.MessageId != id {
		t.Fatalf("closing frame %+v, reply message %s", last, msgs[1].ID)
	}
	if last.Text != "Hello world" || len(last.Tools) != 1 {
		t.Fatalf("closing frame drops the final text and tool lines: %+v", last)
	}
	prevText := ""
	for i, f := range frames[:len(frames)-1] {
		if !f.Active || f.MessageId != id {
			t.Fatalf("frame %d = %+v", i, f)
		}
		if i > 0 && f.Seq <= frames[i-1].Seq {
			t.Fatalf("seq went %d -> %d", frames[i-1].Seq, f.Seq)
		}
		if !strings.HasPrefix(f.Text, prevText) {
			t.Fatalf("text shrank: %q -> %q", prevText, f.Text)
		}
		prevText = f.Text
	}
	if !strings.HasPrefix("Hello world", prevText) || len(prevText) < len("Hello wor") {
		t.Fatalf("the last live text %q", prevText)
	}
	if first := frames[0]; first.Status.Kind != api.SessionProgressStatusKindThinking {
		t.Fatalf("the first frame %+v, want Thinking", first)
	}
}

func TestLiveUpdatesInABurstAreCoalesced(t *testing.T) {
	e := newEnv(t)
	live := e.withProgress(300 * time.Millisecond)
	steps := []kagenttest.Step{kagenttest.Emit{Event: agentArtifact("a1", "w", false)}}
	for range 40 {
		steps = append(steps, kagenttest.Emit{Event: agentArtifact("a1", "x", true)})
	}
	final := &a2a.Task{
		ID: "t-live", ContextID: "ctx", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart("w" + strings.Repeat("x", 40))}}},
	}
	steps = append(steps, kagenttest.Pause{D: 700 * time.Millisecond}, kagenttest.Emit{Event: final})
	e.srv.Script(steps...)
	e.srv.AddTask(final)
	e.run()
	id := e.send("hi")
	live.waitFor(t, "all the text", func(f api.EventSessionProgress) bool {
		return f.Active && f.Text == "w"+strings.Repeat("x", 40)
	})
	if n := len(live.all()); n > 4 {
		t.Fatalf("%d frames for a 41-update burst inside one interval", n)
	}
	e.waitState(id, dbstate.DeliveryCompleted)
}

func TestALateJoinerGetsTheCurrentPartialStateFromTheSnapshot(t *testing.T) {
	e := newEnv(t)
	live := e.withProgress(20 * time.Millisecond)
	running := &a2a.Task{ID: "t-live", ContextID: "ctx", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	e.srv.Script(
		kagenttest.Emit{Event: running}, kagenttest.Emit{Event: agentArtifact("a1", "half a rep", false)},
		kagenttest.Hang{},
	)
	e.run()
	id := e.send("hi")
	live.waitFor(t, "the partial text", func(f api.EventSessionProgress) bool { return f.Text == "half a rep" })

	snap := e.svc.Progress.Snapshot(owner)
	if len(snap) != 1 || !snap[0].Live {
		t.Fatalf("snapshot %+v", snap)
	}
	var f api.EventSessionProgress
	if err := json.Unmarshal(snap[0].Payload, &f); err != nil {
		t.Fatal(err)
	}
	if !f.Active || f.MessageId != id || f.Text != "half a rep" || f.StartedAt == "" {
		t.Fatalf("snapshot frame %+v", f)
	}
	if got := len(e.svc.Progress.Snapshot("someone-else")); got != 0 {
		t.Fatalf("another user's snapshot has %d frames", got)
	}
}

func TestADroppedLiveStreamFallsBackToTheDurableOutcome(t *testing.T) {
	e := newEnv(t)
	live := e.withProgress(20 * time.Millisecond)
	e.srv.Script(kagenttest.Emit{Event: agentArtifact("a1", "partial", false)}, kagenttest.Drop{})
	e.run()
	id := e.send("hi")
	e.waitState(id, dbstate.DeliveryUncertain)
	live.waitFor(t, "the partial text", func(f api.EventSessionProgress) bool { return f.Text == "partial" })

	// The message did reach kagent; the lookup adopts the finished task, as it does without progress.
	e.srv.AddTask(completedTask("t-live", e.kagentSessionID(), id, "the real reply"))
	e.waitState(id, dbstate.DeliveryCompleted)
	live.waitFor(t, "the closing frame", func(f api.EventSessionProgress) bool { return !f.Active })

	msgs := e.messages()
	if len(msgs) != 2 || msgs[1].Content != "the real reply" || e.sends() != 1 {
		t.Fatalf("messages %+v, %d sends", msgs, e.sends())
	}
	frames := live.all()
	if last := frames[len(frames)-1]; last.Active || last.ReplyId == nil || *last.ReplyId != msgs[1].ID {
		t.Fatalf("closing frame %+v", last)
	}
	if n := len(e.svc.Progress.Snapshot(owner)); n != 0 {
		t.Fatalf("%d turns still live after the settle", n)
	}
}

func TestATurnThatFailsClosesTheLiveBubbleWithoutAReply(t *testing.T) {
	e := newEnv(t)
	live := e.withProgress(20 * time.Millisecond)
	failed := &a2a.Task{ID: "t-live", ContextID: "ctx", Status: a2a.TaskStatus{State: a2a.TaskStateFailed}}
	e.srv.Script(kagenttest.Emit{Event: agentArtifact("a1", "partial", false)}, kagenttest.Emit{Event: failed})
	e.srv.AddTask(failed)
	e.run()
	id := e.send("hi")
	e.waitState(id, dbstate.DeliveryFailed)
	live.waitFor(t, "the closing frame", func(f api.EventSessionProgress) bool { return !f.Active })
	frames := live.all()
	last := frames[len(frames)-1]
	if last.MessageId != id || last.Active {
		t.Fatalf("closing frame %+v", last)
	}
	if n := len(e.svc.Progress.Snapshot(owner)); n != 0 {
		t.Fatalf("%d turns still live", n)
	}
}
