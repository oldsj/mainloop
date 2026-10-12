package progress_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/progress"
)

type capture struct {
	mu     sync.Mutex
	frames []api.EventSessionProgress
	users  []string
}

func (c *capture) DispatchLive(userID string, typ events.Type, payload json.RawMessage) {
	if typ != events.TypeSessionProgress {
		panic("unexpected type " + string(typ))
	}
	var f api.EventSessionProgress
	if err := json.Unmarshal(payload, &f); err != nil {
		panic(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, f)
	c.users = append(c.users, userID)
}

func (c *capture) all() []api.EventSessionProgress {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]api.EventSessionProgress(nil), c.frames...)
}

func (c *capture) last(t *testing.T) api.EventSessionProgress {
	t.Helper()
	f := c.all()
	if len(f) == 0 {
		t.Fatal("no frames")
	}
	return f[len(f)-1]
}

func (c *capture) waitFrames(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(c.all()) < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(c.all()); got < n {
		t.Fatalf("%d frames, want %d", got, n)
	}
}

var ref = progress.Ref{UserID: "u", SessionID: "s", MessageID: "m1", Started: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}

func text(artifact, s string, appendTo bool) *a2a.TaskArtifactUpdateEvent {
	ev := a2a.NewArtifactEvent(&a2a.Task{ID: "t", ContextID: "c"}, a2a.NewTextPart(s))
	ev.Artifact.ID = a2a.ArtifactID(artifact)
	ev.Append = appendTo
	return ev
}

func toolPart(kind string, data map[string]any) *a2a.TaskArtifactUpdateEvent {
	p := a2a.NewDataPart(data)
	p.Metadata = map[string]any{"kagent.dev/a2a/part-type": kind}
	ev := a2a.NewArtifactEvent(&a2a.Task{ID: "t", ContextID: "c"}, p)
	ev.Artifact.ID = a2a.ArtifactID(kind + "-" + data["id"].(string))
	return ev
}

func call(id, name string, args map[string]any) *a2a.TaskArtifactUpdateEvent {
	return toolPart("function_call", map[string]any{"id": id, "name": name, "args": args})
}

func result(id, name string, isErr bool) *a2a.TaskArtifactUpdateEvent {
	resp := map[string]any{"result": "x"}
	if isErr {
		resp["isError"] = true
	}
	return toolPart("function_response", map[string]any{"id": id, "name": name, "response": resp})
}

func newTracker(pub progress.Publisher, interval time.Duration) *progress.Tracker {
	return progress.New(pub, progress.WithInterval(interval))
}

func TestBeginShowsThinkingAtOnce(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, time.Hour)
	tr.Begin(ref)
	f := c.last(t)
	if !f.Active || f.Seq != 1 || f.Status.Kind != api.SessionProgressStatusKindThinking || f.Status.Label != "Thinking…" ||
		f.MessageId != "m1" || f.SessionId != "s" || f.Text != "" || len(f.Tools) != 0 ||
		f.StartedAt != "2026-10-11T12:00:00" {
		t.Fatalf("frame %+v", f)
	}
}

func TestTextGrowsByAppendAndIsReplacedWithoutIt(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 0)
	tr.Observe(ref, text("a1", "Hello", false))
	tr.Observe(ref, text("a1", " world", true))
	if f := c.last(t); f.Text != "Hello world" || f.Status.Kind != api.SessionProgressStatusKindWriting {
		t.Fatalf("frame %+v", f)
	}
	tr.Observe(ref, text("a1", "Replaced", false))
	tr.Observe(ref, text("a2", "Second", false))
	if f := c.last(t); f.Text != "Replaced\n\nSecond" {
		t.Fatalf("text %q", f.Text)
	}
	seqs := c.all()
	for i := 1; i < len(seqs); i++ {
		if seqs[i].Seq <= seqs[i-1].Seq {
			t.Fatalf("seq not increasing: %+v", seqs)
		}
	}
}

func TestToolCallsShowLabelsAndStates(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 0)
	tr.Observe(ref, call("c1", "Bash", map[string]any{"command": "go test ./..."}))
	f := c.last(t)
	if f.Status.Kind != api.SessionProgressStatusKindTool || f.Status.Label != "Running Bash: go test ./..." ||
		len(f.Tools) != 1 || f.Tools[0].State != api.SessionProgressToolStateRunning {
		t.Fatalf("frame %+v", f)
	}
	tr.Observe(ref, result("c1", "Bash", false))
	tr.Observe(ref, call("c2", "Read", map[string]any{"file_path": "main.go"}))
	if f = c.last(t); f.Status.Label != "Reading main.go" || f.Tools[0].State != api.SessionProgressToolStateDone ||
		f.Tools[1].State != api.SessionProgressToolStateRunning {
		t.Fatalf("frame %+v", f)
	}
	tr.Observe(ref, result("c2", "Read", true))
	if f = c.last(t); f.Tools[1].State != api.SessionProgressToolStateError || f.Status.Kind != api.SessionProgressStatusKindThinking {
		t.Fatalf("frame %+v", f)
	}
}

func TestLabels(t *testing.T) {
	long := strings.Repeat("x", 300)
	for _, tc := range []struct {
		name string
		args any
		want string
	}{
		{"Grep", map[string]any{"pattern": "TODO"}, "Searching for TODO"},
		{"Edit", map[string]any{"file_path": "a/b.go"}, "Editing a/b.go"},
		{"exec_command", map[string]any{"cmd": []any{"ls", "-la"}}, "Running Bash: ls -la"},
		{"Bash", map[string]any{"command": "echo\n  hi"}, "Running Bash: echo hi"},
		{"mcp_thing", map[string]any{"query": "q"}, "Running mcp_thing: q"},
		{"mcp_thing", nil, "Running mcp_thing"},
		{"", nil, "Running a tool"},
	} {
		if got := progress.Label(tc.name, tc.args); got != tc.want {
			t.Errorf("Label(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := []rune(progress.Label("Bash", map[string]any{"command": long})); len(got) != 120 || got[119] != '…' {
		t.Fatalf("long label has %d runes", len(got))
	}
}

func TestWaitingStatusFromInputRequired(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 0)
	tr.Observe(ref, a2a.NewStatusUpdateEvent(&a2a.Task{ID: "t", ContextID: "c"}, a2a.TaskStateInputRequired, nil))
	if f := c.last(t); f.Status.Kind != api.SessionProgressStatusKindWaiting || f.Status.Label != "Waiting for you" {
		t.Fatalf("frame %+v", f)
	}
	tr.Observe(ref, text("a1", "carrying on", false))
	if f := c.last(t); f.Status.Kind != api.SessionProgressStatusKindWriting {
		t.Fatalf("frame %+v", f)
	}
}

func TestSnapshotTaskRebuildsStateForATakenOverTurn(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 0)
	tr.Observe(ref, &a2a.Task{
		ID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		Artifacts: []*a2a.Artifact{{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart("so far")}}},
	})
	if f := c.last(t); f.Text != "so far" {
		t.Fatalf("frame %+v", f)
	}
}

func TestUpdatesAreCoalescedToOneTrailingFramePerInterval(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 80*time.Millisecond)
	tr.Observe(ref, text("a1", "a", false))
	for _, s := range []string{"b", "c", "d", "e"} {
		tr.Observe(ref, text("a1", s, true))
	}
	if n := len(c.all()); n != 1 {
		t.Fatalf("%d frames before the interval passed", n)
	}
	c.waitFrames(t, 2)
	time.Sleep(150 * time.Millisecond)
	fs := c.all()
	if len(fs) != 2 || fs[0].Text != "a" || fs[1].Text != "abcde" {
		t.Fatalf("frames %+v", fs)
	}
}

func TestTextIsBoundedKeepingTheEnd(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 0)
	tr.Observe(ref, text("a1", strings.Repeat("é", progress.MaxText), false))
	tr.Observe(ref, text("a1", "TAIL", true))
	f := c.last(t)
	if !f.TextTruncated || len(f.Text) > progress.MaxText || !strings.HasSuffix(f.Text, "TAIL") {
		t.Fatalf("truncated=%v len=%d", f.TextTruncated, len(f.Text))
	}
}

func TestToolLinesAreBounded(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 0)
	for i := range progress.MaxTools + 5 {
		id := string(rune('A' + i))
		tr.Observe(ref, call(id, "Read", map[string]any{"file_path": id}))
	}
	f := c.last(t)
	if len(f.Tools) != progress.MaxTools || f.Tools[len(f.Tools)-1].Label != "Reading "+string(rune('A'+progress.MaxTools+4)) {
		t.Fatalf("%d tools", len(f.Tools))
	}
}

func TestSnapshotFramesAreForTheUserAndNewerThanWhatWasSent(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 0)
	tr.Observe(ref, text("a1", "partial", false))
	other := progress.Ref{UserID: "other", SessionID: "s2", MessageID: "m9"}
	tr.Observe(other, text("a1", "not yours", false))
	snap := tr.Snapshot("u")
	if len(snap) != 1 || !snap[0].Live || snap[0].Type != events.TypeSessionProgress || snap[0].UserID != "u" {
		t.Fatalf("snapshot %+v", snap)
	}
	var f api.EventSessionProgress
	if err := json.Unmarshal(snap[0].Payload, &f); err != nil {
		t.Fatal(err)
	}
	if f.Text != "partial" || f.Seq != c.last(t).Seq+1 {
		t.Fatalf("snapshot frame %+v", f)
	}
	if len(tr.Snapshot("nobody")) != 0 {
		t.Fatal("snapshot leaked to another user")
	}
}

func TestATaskSnapshotWithoutToolArtifactsKeepsTheToolLinesAlreadySeen(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 0)
	tr.Observe(ref, call("c1", "Read", map[string]any{"file_path": "main.go"}))
	tr.Observe(ref, result("c1", "Read", false))
	tr.Observe(ref, &a2a.Task{
		ID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart("the reply")}}},
	})
	f := c.last(t)
	if f.Text != "the reply" || len(f.Tools) != 1 || f.Tools[0].Label != "Reading main.go" {
		t.Fatalf("frame %+v", f)
	}
}

func TestEndSendsAFinalFrameNamingTheReplyAndForgetsTheTurn(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 0)
	tr.Observe(ref, text("a1", "partial", false))
	tr.End(ref, "reply-1")
	f := c.last(t)
	if f.Active || f.ReplyId == nil || *f.ReplyId != "reply-1" || f.Text != "partial" {
		t.Fatalf("frame %+v", f)
	}
	if len(tr.Snapshot("u")) != 0 {
		t.Fatal("ended turn still in the snapshot")
	}
	// A late event of the ended turn does not bring it back; the next message's turn is tracked.
	n := len(c.all())
	tr.Observe(ref, text("a1", "late", true))
	if len(c.all()) != n || len(tr.Snapshot("u")) != 0 {
		t.Fatal("a late event revived the turn")
	}
	next := ref
	next.MessageID = "m2"
	tr.Begin(next)
	if f := c.last(t); !f.Active || f.MessageId != "m2" {
		t.Fatalf("frame %+v", f)
	}
}

func TestTheClosingFrameKeepsTheLastTextAndToolLines(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 0)
	tr.Observe(ref, text("a1", "All done.", false))
	tr.Observe(ref, call("c1", "Read", map[string]any{"file_path": "main.go"}))
	tr.Observe(ref, result("c1", "Read", false))
	live := c.last(t)
	tr.End(ref, "reply-1")
	f := c.last(t)
	if f.Active || f.Text != live.Text || f.TextTruncated != live.TextTruncated || len(f.Tools) != 1 ||
		f.Tools[0].Label != "Reading main.go" || f.Seq <= live.Seq || f.StartedAt != live.StartedAt {
		t.Fatalf("closing frame %+v, last live frame %+v", f, live)
	}
}

func TestEndWithoutAReplyOmitsReplyID(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 0)
	tr.Begin(ref)
	tr.End(ref, "")
	if f := c.last(t); f.Active || f.ReplyId != nil {
		t.Fatalf("frame %+v", f)
	}
}

func TestEndOfAnUntrackedTurnIsSilent(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 0)
	tr.End(ref, "r")
	if len(c.all()) != 0 {
		t.Fatalf("frames %+v", c.all())
	}
}

func TestEndCancelsAPendingTrailingFrame(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 60*time.Millisecond)
	tr.Observe(ref, text("a1", "a", false))
	tr.Observe(ref, text("a1", "b", true))
	tr.End(ref, "r")
	time.Sleep(150 * time.Millisecond)
	fs := c.all()
	if len(fs) != 2 || fs[1].Active {
		t.Fatalf("frames after End: %+v", fs)
	}
}

func TestANilTrackerDoesNothing(t *testing.T) {
	var tr *progress.Tracker
	tr.Begin(ref)
	tr.Observe(ref, text("a", "x", false))
	tr.End(ref, "")
	if tr.Snapshot("u") != nil {
		t.Fatal("snapshot from nil tracker")
	}
}

func TestAQuietTurnIsNotForgottenAndItsSeqNeverRestarts(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	c := &capture{}
	tr := progress.New(c, progress.WithInterval(0), progress.WithClock(func() time.Time { return now }))
	tr.Observe(ref, text("a1", "Here is my plan.", false))
	tr.Observe(ref, call("c1", "Bash", map[string]any{"command": "go test ./..."}))
	before := c.last(t)

	// kagent says nothing while the tool runs, however long that takes.
	now = now.Add(40 * time.Minute)
	snap := tr.Snapshot("u")
	if len(snap) != 1 {
		t.Fatalf("snapshot has %d turns after a long quiet spell, want the running one", len(snap))
	}
	tr.Observe(ref, result("c1", "Bash", false))
	after := c.last(t)
	if after.Seq <= before.Seq {
		t.Fatalf("seq went from %d to %d: a message's seq must not restart", before.Seq, after.Seq)
	}
	if after.Text != "Here is my plan." || len(after.Tools) != 1 ||
		after.Tools[0].Label != "Running Bash: go test ./..." || after.Tools[0].State != api.SessionProgressToolStateDone {
		t.Fatalf("state was lost across the quiet spell: %+v", after)
	}
}

func TestOnlyEndOrANewerMessageRemovesATurn(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 0)
	tr.Observe(ref, text("a1", "first", false))
	next := ref
	next.MessageID = "m2"
	tr.Observe(next, text("a1", "second", false))
	f := c.last(t)
	if f.MessageId != "m2" || f.Text != "second" || f.Seq != 1 {
		t.Fatalf("a newer message did not replace the turn: %+v", f)
	}
	if n := len(tr.Snapshot("u")); n != 1 {
		t.Fatalf("snapshot has %d turns, want 1", n)
	}
	tr.End(ref, "") // the replaced message's late end changes nothing
	if n := len(tr.Snapshot("u")); n != 1 {
		t.Fatalf("a stale End removed the newer turn")
	}
}

func TestAResultWithNoStartIsLabelledWithoutAColon(t *testing.T) {
	c := &capture{}
	tr := newTracker(c, 0)
	tr.Observe(ref, result("c9", "Bash", false))
	if f := c.last(t); len(f.Tools) != 1 || f.Tools[0].Label != "Running Bash" || f.Tools[0].State != api.SessionProgressToolStateDone {
		t.Fatalf("frame %+v", f)
	}
}
