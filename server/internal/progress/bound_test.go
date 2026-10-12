package progress

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/events"
)

func appendText(artifact, s string) *a2a.TaskArtifactUpdateEvent {
	ev := a2a.NewArtifactEvent(&a2a.Task{ID: "t", ContextID: "c"}, a2a.NewTextPart(s))
	ev.Artifact.ID = a2a.ArtifactID(artifact)
	ev.Append = true
	return ev
}

func toolCall(id string) *a2a.TaskArtifactUpdateEvent {
	p := a2a.NewDataPart(map[string]any{"id": id, "name": "Read", "args": map[string]any{"path": id}})
	p.Metadata = map[string]any{partTypeKey: callKind}
	ev := a2a.NewArtifactEvent(&a2a.Task{ID: "t", ContextID: "c"}, p)
	ev.Artifact.ID = a2a.ArtifactID("call-" + id)
	return ev
}

type sink struct{}

func (sink) DispatchLive(string, events.Type, json.RawMessage) {}

func TestStoredStateIsBoundedNotJustTheFrame(t *testing.T) {
	tr := New(sink{}, WithInterval(0))
	ref := Ref{UserID: "u", SessionID: "s", MessageID: "m"}
	chunk := strings.Repeat("é", 512) // multibyte, so a cut has to land on a rune start
	for i := range 2000 {
		tr.Observe(ref, appendText("a", chunk))
		tr.Observe(ref, toolCall("c"+strconv.Itoa(i)))
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	cur := tr.turns["s"]
	var tools, bytes int
	for _, e := range cur.entries {
		if e.tool != nil {
			tools++
		}
		bytes += len(e.text)
		if !utf8.ValidString(e.text) {
			t.Fatalf("a text entry was cut inside a rune")
		}
	}
	if tools != MaxTools || bytes > MaxText || !cur.cut {
		t.Fatalf("kept %d tools, %d text bytes, cut=%v; want %d, at most %d, true", tools, bytes, cur.cut, MaxTools, MaxText)
	}
	if last := cur.entries[len(cur.entries)-1].tool; last == nil || last.id != "c1999" {
		t.Fatalf("the newest tool line was dropped")
	}
	if body := cur.body(); !body.TextTruncated {
		t.Fatalf("frame does not say the text is cut")
	}
}

func TestTheEndedMessageMemoryIsBounded(t *testing.T) {
	tr := New(sink{}, WithInterval(0))
	for i := range 3 * maxDone {
		tr.End(Ref{UserID: "u", SessionID: "s" + strconv.Itoa(i), MessageID: "m"}, "")
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.done) > maxDone || len(tr.order) > maxDone {
		t.Fatalf("done has %d entries (order %d), want at most %d", len(tr.done), len(tr.order), maxDone)
	}
	// The newest are the ones kept: a late event of a just-ended turn must still not revive it.
	last := "s" + strconv.Itoa(3*maxDone-1)
	if tr.done[last] != "m" {
		t.Fatalf("the newest ended message was forgotten")
	}
}
