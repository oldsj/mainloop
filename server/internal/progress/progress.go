// Package progress is the live state of running session turns: the reply as it grows, the current
// step and the tool calls, published as the contract's `session:progress` event.
//
// It is ephemeral by design. The durable record of a turn is still the mirrored reply the delivery
// settle writes once; this package only shows what the agent is doing until then. State lives in
// memory, one entry per session with a turn in flight, overwritten by each update and capped in size
// (the text at MaxText bytes, the tool lines at MaxTools, both in what is kept as well as in what is
// sent). A turn stays until [Tracker.End] or a newer message of the same session replaces it: kagent
// says nothing while a tool runs, so quiet is not a reason to forget it. Nothing here is stored or replayed: a
// restart loses it and the owner sees today's behaviour (a busy flag, then the reply). A database row
// would add a write per update to a path that must stay cheap, to keep a value nobody needs after the
// turn ends. The cost of memory is that the delivery loop and the API must share the Tracker, so live
// state is single-process; serving the API from another replica would need a channel between them.
//
// Each event is the whole state of one turn, not a delta, so a dropped or coalesced frame loses
// nothing and a client that joins late is brought up to date with one frame (see [Tracker.Snapshot]).
// Updates are coalesced to at most one frame per interval per session: the first goes out at once,
// later ones within the interval are folded into one trailing frame.
package progress

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/events"
)

const (
	// MaxText bounds the reply text in a frame. The end is kept.
	MaxText = 16 << 10
	// MaxTools bounds the tool lines in a frame. The latest are kept.
	MaxTools = 30
	// DefaultInterval is the shortest time between frames of one session: four a second.
	DefaultInterval = 250 * time.Millisecond
	// maxDone bounds how many sessions' last ended message are remembered, to keep a late event from
	// reviving a turn. Late events arrive within moments of the end, so the oldest are safe to forget.
	maxDone = 1024

	maxLabel = 120

	// partTypeKey marks a kagent tool-activity data part; see kagent's api/a2a.
	partTypeKey = "kagent.dev/a2a/part-type"
	callKind    = "function_call"
	resultKind  = "function_response"
)

// Publisher sends a live event to a user's streams. *events.Hub is one.
type Publisher interface {
	DispatchLive(userID string, typ events.Type, payload json.RawMessage)
}

// Ref names a turn: the user and session it belongs to and the delivery message it answers.
type Ref struct {
	UserID, SessionID, MessageID string
	// Started is when the turn began; the elapsed time shown is measured from it.
	Started time.Time
}

// Option tunes a Tracker.
type Option func(*Tracker)

// WithInterval sets the shortest time between frames of one session.
func WithInterval(d time.Duration) Option { return func(t *Tracker) { t.interval = d } }

// WithClock replaces time.Now. For tests.
func WithClock(now func() time.Time) Option { return func(t *Tracker) { t.now = now } }

// Tracker holds the live state of the running turns. A nil *Tracker accepts every call and does
// nothing, so a process that serves no events needs no stand-in.
type Tracker struct {
	pub      Publisher
	interval time.Duration
	now      func() time.Time

	mu    sync.Mutex
	turns map[string]*turn  // by session id
	done  map[string]string // session id -> the last message whose turn ended, so a late event cannot revive it
	order []string          // the keys of done, oldest first, to bound it
}

// New builds a Tracker that publishes through pub.
func New(pub Publisher, opts ...Option) *Tracker {
	t := &Tracker{pub: pub, interval: DefaultInterval, now: time.Now,
		turns: map[string]*turn{}, done: map[string]string{}}
	for _, o := range opts {
		o(t)
	}
	return t
}

type entry struct {
	artifact string // the artifact id of a text entry
	text     string
	tool     *tool // set for a tool line
}

type tool struct {
	id, name, label string
	state           api.SessionProgressToolState
}

type turn struct {
	ref     Ref
	seq     int
	entries []entry
	waiting bool
	cut     bool // bound dropped the start of the text

	lastSent time.Time
	dirty    bool
	timer    *time.Timer
}

// Begin starts showing a turn as soon as its message is being sent, so the owner sees "Thinking…"
// before kagent says anything. Beginning a turn that is already tracked keeps its state.
func (t *Tracker) Begin(ref Ref) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if tr := t.turnLocked(ref); tr != nil {
		t.markLocked(tr)
	}
}

// Observe folds one kagent task event into the turn and publishes the result, coalesced.
func (t *Tracker) Observe(ref Ref, ev a2a.Event) {
	if t == nil || ev == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	tr := t.turnLocked(ref)
	if tr == nil {
		return
	}
	switch e := ev.(type) {
	case *a2a.Task:
		tr.snapshot(e)
	case *a2a.TaskStatusUpdateEvent:
		tr.waiting = waits(e.Status.State)
	case *a2a.TaskArtifactUpdateEvent:
		tr.waiting = false
		tr.artifact(e.Artifact, e.Append)
	default:
		return
	}
	tr.bound()
	t.markLocked(tr)
}

// End removes the turn after its final state is stored, and tells the streams. The closing frame
// keeps the last text and tool lines, so a page that shows it until the stored reply has been read
// does not blank in between. replyID is the id of the conversation message that now holds the
// turn's outcome, or empty when the turn wrote none.
// Ending a turn that is not tracked does nothing: no client has live state for it.
func (t *Tracker) End(ref Ref, replyID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endLocked(ref, replyID)
}

// EndMessage ends the turn of a delivery message whose session is gone, so the caller knows the
// message id but not the session. It does nothing when no turn of that message is tracked.
func (t *Tracker) EndMessage(messageID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tr := range t.turns {
		if tr.ref.MessageID == messageID {
			t.endLocked(tr.ref, "")
			return
		}
	}
}

func (t *Tracker) endLocked(ref Ref, replyID string) {
	t.markDoneLocked(ref.SessionID, ref.MessageID)
	tr, ok := t.turns[ref.SessionID]
	if !ok || tr.ref.MessageID != ref.MessageID {
		return
	}
	delete(t.turns, ref.SessionID)
	if tr.timer != nil {
		tr.timer.Stop()
	}
	tr.seq++
	body := tr.body()
	body.Active = false
	if replyID != "" {
		body.ReplyId = &replyID
	}
	t.publish(ref.UserID, body)
}

// Snapshot returns one frame for each running turn of userID, for a stream that has just connected.
func (t *Tracker) Snapshot(userID string) []events.Event {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []events.Event
	for _, tr := range t.turns {
		if tr.ref.UserID != userID {
			continue
		}
		tr.seq++
		raw, err := json.Marshal(tr.body())
		if err != nil {
			continue
		}
		out = append(out, events.Event{UserID: userID, Type: events.TypeSessionProgress, Payload: raw, Live: true})
	}
	return out
}

func (t *Tracker) turnLocked(ref Ref) *turn {
	if ref.SessionID == "" || ref.MessageID == "" {
		return nil
	}
	if t.done[ref.SessionID] == ref.MessageID {
		return nil
	}
	tr, ok := t.turns[ref.SessionID]
	if ok && tr.ref.MessageID == ref.MessageID {
		return tr
	}
	if ok { // a newer message of the session: the old turn is over whether or not it said so
		t.dropLocked(ref.SessionID, tr)
	}
	tr = &turn{ref: ref}
	if tr.ref.Started.IsZero() {
		tr.ref.Started = t.now()
	}
	t.turns[ref.SessionID] = tr
	return tr
}

// markDoneLocked remembers that a message's turn ended, keeping at most maxDone sessions. A session
// that ends another turn moves to the back of the order, so the oldest to forget is the one that
// has gone longest without a turn ending.
func (t *Tracker) markDoneLocked(sid, mid string) {
	if _, ok := t.done[sid]; ok {
		if i := slices.Index(t.order, sid); i >= 0 {
			t.order = slices.Delete(t.order, i, i+1)
		}
	} else if len(t.order) >= maxDone {
		delete(t.done, t.order[0])
		t.order = t.order[1:]
	}
	t.order = append(t.order, sid)
	t.done[sid] = mid
}

func (t *Tracker) dropLocked(sid string, tr *turn) {
	if tr.timer != nil {
		tr.timer.Stop()
	}
	delete(t.turns, sid)
}

// markLocked publishes the turn now, or, when it published less than an interval ago, once at the end
// of the interval with everything that arrived meanwhile.
func (t *Tracker) markLocked(tr *turn) {
	tr.dirty = true
	if tr.timer != nil {
		return
	}
	wait := t.interval - t.now().Sub(tr.lastSent)
	if wait <= 0 {
		t.flushLocked(tr)
		return
	}
	tr.timer = time.AfterFunc(wait, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		tr.timer = nil
		if t.turns[tr.ref.SessionID] == tr {
			t.flushLocked(tr)
		}
	})
}

func (t *Tracker) flushLocked(tr *turn) {
	if !tr.dirty {
		return
	}
	tr.dirty = false
	tr.lastSent = t.now()
	tr.seq++
	t.publish(tr.ref.UserID, tr.body())
}

func (t *Tracker) publish(userID string, body api.EventSessionProgress) {
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	t.pub.DispatchLive(userID, events.TypeSessionProgress, raw)
}

// waits reports whether a task state is a pause for the owner.
func waits(s a2a.TaskState) bool {
	switch norm(s) {
	case "input_required", "auth_required":
		return true
	}
	return false
}

func norm(s a2a.TaskState) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(string(s), "TASK_STATE_")), "-", "_")
}

// snapshot replaces the turn's content with a task's artifacts: the first event of a subscription is
// the task as it stands, so a handler that took the turn over starts from the right text.
func (tr *turn) snapshot(task *a2a.Task) {
	old := tr.entries
	tr.entries = nil
	tr.cut = false
	for _, a := range task.Artifacts {
		tr.artifact(a, false)
	}
	// A task that lists no tool artifact (the final one may carry only the reply) does not erase the
	// tool lines already seen: keep those the snapshot does not mention, as the older lines.
	var seen = map[string]bool{}
	for _, e := range tr.entries {
		if e.tool != nil {
			seen[e.tool.id] = true
		}
	}
	var lines []entry
	for _, e := range old {
		if e.tool != nil && !seen[e.tool.id] {
			lines = append(lines, e)
		}
	}
	tr.entries = append(lines, tr.entries...)
	tr.waiting = waits(task.Status.State)
	tr.bound()
}

// bound keeps the stored state within the caps the frame has: the newest MaxText bytes of text and
// the newest MaxTools tool lines. Without it a long turn would grow without limit and every frame
// would cost the whole turn.
func (tr *turn) bound() {
	tools, budget := 0, MaxText
	keep := make([]bool, len(tr.entries))
	for i := len(tr.entries) - 1; i >= 0; i-- {
		e := &tr.entries[i]
		if e.tool != nil {
			keep[i] = tools < MaxTools
			tools++
			continue
		}
		switch {
		case budget <= 0:
			tr.cut = true
		case len(e.text) <= budget:
			budget -= len(e.text)
			keep[i] = true
		default:
			cut := len(e.text) - budget
			for cut < len(e.text) && !utf8.RuneStart(e.text[cut]) {
				cut++
			}
			e.text = e.text[cut:]
			budget = 0
			tr.cut = true
			keep[i] = true
		}
	}
	kept := tr.entries[:0]
	for i, e := range tr.entries {
		if keep[i] {
			kept = append(kept, e)
		}
	}
	clear(tr.entries[len(kept):])
	tr.entries = kept
}

// artifact folds one artifact (or an appended chunk of one) into the entries.
func (tr *turn) artifact(a *a2a.Artifact, appendTo bool) {
	if a == nil {
		return
	}
	var text strings.Builder
	for _, p := range a.Parts {
		if p == nil {
			continue
		}
		if kind, _ := p.Metadata[partTypeKey].(string); kind == callKind || kind == resultKind {
			tr.toolPart(kind, p.Data())
			continue
		}
		text.WriteString(p.Text())
	}
	if text.Len() == 0 {
		return
	}
	id := string(a.ID)
	for i := range tr.entries {
		if e := &tr.entries[i]; e.tool == nil && e.artifact == id && id != "" {
			if appendTo {
				e.text += text.String()
			} else {
				e.text = text.String()
			}
			return
		}
	}
	tr.entries = append(tr.entries, entry{artifact: id, text: text.String()})
}

// ToolPart reports whether p is a kagent tool-activity part, with its call id and whether it starts
// the call (a function call) rather than ends it (the call's response).
func ToolPart(p *a2a.Part) (id string, starts, ok bool) {
	if p == nil {
		return "", false, false
	}
	kind, _ := p.Metadata[partTypeKey].(string)
	if kind != callKind && kind != resultKind {
		return "", false, false
	}
	m, _ := p.Data().(map[string]any)
	id, _ = m["id"].(string)
	return id, kind == callKind, true
}

func (tr *turn) toolPart(kind string, data any) {
	m, _ := data.(map[string]any)
	id, _ := m["id"].(string)
	name, _ := m["name"].(string)
	if kind == callKind {
		tr.entries = append(tr.entries, entry{tool: &tool{
			id: id, name: name, label: Label(name, m["args"]), state: api.SessionProgressToolStateRunning,
		}})
		return
	}
	state := api.SessionProgressToolStateDone
	if resp, ok := m["response"].(map[string]any); ok {
		if bad, _ := resp["isError"].(bool); bad {
			state = api.SessionProgressToolStateError
		}
	}
	for i := len(tr.entries) - 1; i >= 0; i-- {
		if e := tr.entries[i]; e.tool != nil && e.tool.id == id {
			e.tool.state = state
			return
		}
	}
	tr.entries = append(tr.entries, entry{tool: &tool{id: id, name: name, label: Label(name, nil), state: state}})
}

// text is the reply so far: the text entries, trimmed, joined as the settle joins artifacts, so the
// live bubble and the stored reply read the same.
func (tr *turn) text() (string, bool) {
	var parts []string
	for _, e := range tr.entries {
		if s := strings.TrimSpace(e.text); e.tool == nil && s != "" {
			parts = append(parts, s)
		}
	}
	s := strings.Join(parts, "\n\n")
	if len(s) <= MaxText {
		return s, tr.cut
	}
	cut := len(s) - MaxText
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:], true
}

func status(tr *turn) api.SessionProgressStatus {
	if tr.waiting {
		return api.SessionProgressStatus{Kind: api.SessionProgressStatusKindWaiting, Label: "Waiting for you"}
	}
	if n := len(tr.entries); n > 0 {
		last := tr.entries[n-1]
		switch {
		case last.tool != nil && last.tool.state == api.SessionProgressToolStateRunning:
			return api.SessionProgressStatus{Kind: api.SessionProgressStatusKindTool, Label: last.tool.label}
		case last.tool == nil && strings.TrimSpace(last.text) != "":
			return api.SessionProgressStatus{Kind: api.SessionProgressStatusKindWriting, Label: "Writing…"}
		}
	}
	return api.SessionProgressStatus{Kind: api.SessionProgressStatusKindThinking, Label: "Thinking…"}
}

func (tr *turn) body() api.EventSessionProgress {
	text, cut := tr.text()
	tools := []api.SessionProgressTool{}
	for _, e := range tr.entries {
		if e.tool != nil {
			tools = append(tools, api.SessionProgressTool{
				Id: e.tool.id, Name: e.tool.name, Label: e.tool.label, State: e.tool.state,
			})
		}
	}
	if len(tools) > MaxTools {
		tools = tools[len(tools)-MaxTools:]
	}
	return api.EventSessionProgress{
		SessionId: tr.ref.SessionID, MessageId: tr.ref.MessageID, Seq: tr.seq, Active: true,
		StartedAt: stamp(tr.ref.Started), Status: status(tr), Text: text, TextTruncated: cut, Tools: tools,
	}
}

// stamp is UTC ISO-8601 without an offset, as the contract says.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.999999") }

// Label summarises a tool call in one line: "Running Bash: go test ./...", "Reading file.go".
func Label(name string, args any) string {
	a, _ := args.(map[string]any)
	str := func(keys ...string) string {
		for _, k := range keys {
			switch v := a[k].(type) {
			case string:
				if s := oneLine(v); s != "" {
					return s
				}
			case []any: // a command as argv
				parts := make([]string, 0, len(v))
				for _, p := range v {
					if s, ok := p.(string); ok {
						parts = append(parts, s)
					}
				}
				if s := oneLine(strings.Join(parts, " ")); s != "" {
					return s
				}
			}
		}
		return ""
	}
	with := func(verb string, detail string) string {
		if detail == "" {
			return strings.TrimSuffix(verb, ":")
		}
		return verb + " " + detail
	}
	switch strings.ToLower(name) {
	case "bash", "shell", "exec_command", "local_shell", "exec":
		return truncate(with("Running Bash:", str("command", "cmd")))
	case "read":
		return truncate(with("Reading", str("file_path", "path")))
	case "write":
		return truncate(with("Writing", str("file_path", "path")))
	case "edit", "multiedit", "apply_patch":
		return truncate(with("Editing", str("file_path", "path")))
	case "grep":
		return truncate(with("Searching for", str("pattern", "query")))
	case "glob":
		return truncate(with("Finding", str("pattern", "path")))
	case "webfetch":
		return truncate(with("Fetching", str("url")))
	case "websearch":
		return truncate(with("Searching the web:", str("query")))
	case "":
		return "Running a tool"
	}
	if d := str("description", "command", "path", "file_path", "query", "url", "pattern"); d != "" {
		return truncate(fmt.Sprintf("Running %s: %s", name, d))
	}
	return truncate("Running " + name)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string) string {
	if utf8.RuneCountInString(s) <= maxLabel {
		return s
	}
	r := []rune(s)
	return string(r[:maxLabel-1]) + "…"
}
