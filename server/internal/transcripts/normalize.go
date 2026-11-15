// Package transcripts copies each finished turn of a kagent Session into Mainloop as transcript
// entries, so what an agent did (its messages, text and tool calls) outlives the kagent Session and
// can be searched (docs/specs/transcripts.md).
//
// kagent records a harness turn as one A2A task: the user's message and status messages in its
// history, the agent's text and each tool call and result as artifacts, ordered by a timeline
// position. [Normalize] turns a task into entries; every text it returns is redacted and bounded,
// so nothing it returns needs further scrubbing before it is stored or indexed.
package transcripts

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/redact"
)

// Kind is what an entry records.
type Kind string

const (
	KindUser       Kind = "user"        // a message the user (or Mainloop for them) sent
	KindAssistant  Kind = "assistant"   // the agent's text
	KindThinking   Kind = "thinking"    // a reasoning summary, where the runtime reports one
	KindToolCall   Kind = "tool_call"   // a tool call and its arguments
	KindToolResult Kind = "tool_result" // a tool call's result
	KindStatus     Kind = "status"      // a status message, such as why a turn failed
	KindAsk        Kind = "ask"         // the question or approval a waiting turn asks for
	KindOther      Kind = "other"       // a part of no known kind, shown as a placeholder
)

// kagent's A2A metadata (kagent go/api/a2a/metadata.go).
const (
	partTypeKey         = "kagent.dev/a2a/part-type"
	timelinePositionKey = "kagent.dev/a2a/timeline-position"
	thoughtKey          = "kagent.dev/a2a/thought"
	callKind            = "function_call"
	resultKind          = "function_response"
	healthPrefix        = "kagent.dev/orchestration-health/"
)

// Withheld replaces the result of a tool call that read a credential file or ran a command that
// prints credentials (see sensitiveCall).
const Withheld = "[withheld: the call reads credentials]"

// Entry is one transcript entry. Text is redacted and bounded.
type Entry struct {
	Pos        int
	Kind       Kind
	ToolName   string
	ToolCallID string
	IsError    bool
	Text       string
	// Truncated is set when Text was cut or withheld; OrigBytes is the size before either.
	Truncated bool
	OrigBytes int
	At        *time.Time
}

// Limits bounds an entry's text, in bytes.
type Limits struct {
	// Text bounds every entry but a tool result; the start is kept.
	Text int
	// Result bounds a tool result; its start and its end are kept.
	Result int
}

// DefaultLimits are the limits the spec states.
var DefaultLimits = Limits{Text: 32 << 10, Result: 16 << 10}

// item is one history message or one artifact of a task.
type item struct {
	message  *a2a.Message
	artifact *a2a.Artifact
}

func (it item) meta() map[string]any {
	if it.artifact != nil {
		return it.artifact.Metadata
	}
	return it.message.Metadata
}

func (it item) parts() a2a.ContentParts {
	if it.artifact != nil {
		return it.artifact.Parts
	}
	return it.message.Parts
}

// Normalize is a task's entries in timeline order. It is deterministic: the same task gives the same
// entries, so a copy that is repeated writes nothing new.
func Normalize(task *a2a.Task, lim Limits) []Entry {
	if task == nil {
		return nil
	}
	n := normalizer{task: task, lim: lim, sensitive: map[string]bool{}}
	for _, it := range timeline(task) {
		n.item(it)
	}
	return n.out
}

type normalizer struct {
	task *a2a.Task
	lim  Limits
	out  []Entry
	// sensitive holds the ids of calls whose result is withheld.
	sensitive map[string]bool
}

func (n *normalizer) item(it item) {
	if a := it.artifact; a != nil && strings.HasPrefix(a.Name, healthPrefix) {
		return // health observations carry no content (internal/health)
	}
	textKind := n.textKind(it)
	at := position(it.meta())
	var run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			n.add(Entry{Kind: textKind, Text: run.String(), At: at})
			run.Reset()
		}
	}
	for _, p := range it.parts() {
		if p == nil {
			continue
		}
		switch kind, _ := p.Metadata[partTypeKey].(string); {
		case kind == callKind || kind == resultKind:
			flush()
			n.tool(kind == callKind, p.Data(), at)
		case isThought(p):
			flush()
			n.add(Entry{Kind: KindThinking, Text: p.Text(), At: at})
		default:
			switch c := p.Content.(type) {
			case a2a.Text:
				run.WriteString(string(c))
			case a2a.Data:
				flush()
				n.add(Entry{Kind: textKind, Text: compactJSON(c.Value), At: at})
			default:
				flush()
				n.add(Entry{Kind: KindOther, Text: placeholder(p), At: at})
			}
		}
	}
	flush()
}

// textKind is the kind of an item's text: the user's message, the agent's text, or the task's
// status message (an ask while the task waits for input).
func (n *normalizer) textKind(it item) Kind {
	if it.artifact != nil {
		return KindAssistant
	}
	if it.message.Role == a2a.MessageRoleUser {
		return KindUser
	}
	if s := n.task.Status.Message; s != nil && (s == it.message || (s.ID != "" && s.ID == it.message.ID)) {
		if n.task.Status.State == a2a.TaskStateInputRequired {
			return KindAsk
		}
		return KindStatus
	}
	return KindAssistant
}

func (n *normalizer) tool(call bool, data any, at *time.Time) {
	m, _ := data.(map[string]any)
	id, _ := m["id"].(string)
	name, _ := m["name"].(string)
	if call {
		if sensitiveCall(m["args"]) && id != "" {
			n.sensitive[id] = true
		}
		args := m["args"]
		if args == nil {
			args = map[string]any{}
		}
		n.add(Entry{Kind: KindToolCall, ToolName: name, ToolCallID: id, Text: compactJSON(args), At: at})
		return
	}
	e := Entry{Kind: KindToolResult, ToolName: name, ToolCallID: id, At: at}
	resp, _ := m["response"].(map[string]any)
	e.IsError, _ = resp["isError"].(bool)
	e.Text = resultText(resp["result"])
	if n.sensitive[id] {
		e.OrigBytes, e.Text, e.Truncated = len(e.Text), Withheld, true
		n.out = append(n.out, n.at(e))
		return
	}
	n.add(e)
}

// add redacts and bounds e's text and appends it. Redaction runs on the whole text before it is cut,
// so a cut can never leave part of a credential that redaction would have recognised.
func (n *normalizer) add(e Entry) {
	if e.Text == "" && e.Kind != KindToolCall && e.Kind != KindToolResult {
		return
	}
	e.OrigBytes = len(e.Text)
	text := redact.Text(e.Text)
	if e.Kind == KindToolResult {
		e.Text, e.Truncated = headTail(text, n.lim.Result)
	} else {
		e.Text, e.Truncated = head(text, n.lim.Text)
	}
	n.out = append(n.out, n.at(e))
}

func (n *normalizer) at(e Entry) Entry {
	e.Pos = len(n.out)
	return e
}

// timeline is a task's messages and artifacts in timeline order, as kagent orders them (kagent
// core/internal/mcp/transcript.go): the history, then the status message unless the history holds
// it, then the artifacts, sorted stably by timeline position when every one has a position.
func timeline(task *a2a.Task) []item {
	var items []item
	status := task.Status.Message
	for _, m := range task.History {
		if m == nil {
			continue
		}
		if status != nil && (m == status || (status.ID != "" && m.ID == status.ID)) {
			status = nil
		}
		items = append(items, item{message: m})
	}
	if status != nil {
		items = append(items, item{message: status})
	}
	for _, a := range task.Artifacts {
		if a != nil {
			items = append(items, item{artifact: a})
		}
	}
	at := make([]time.Time, len(items))
	for i, it := range items {
		p := position(it.meta())
		if p == nil {
			return items
		}
		at[i] = *p
	}
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return at[a].Compare(at[b]) })
	sorted := make([]item, len(items))
	for i, j := range order {
		sorted[i] = items[j]
	}
	return sorted
}

func position(meta map[string]any) *time.Time {
	s, _ := meta[timelinePositionKey].(string)
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return &t
}

func isThought(p *a2a.Part) bool {
	b, _ := p.Metadata[thoughtKey].(bool)
	return b
}

// resultText is a tool result as text: a string as it is, a list of content blocks as their text
// joined, anything else as JSON.
func resultText(v any) string {
	switch r := v.(type) {
	case nil:
		return ""
	case string:
		return r
	case []any:
		var texts []string
		for _, b := range r {
			m, ok := b.(map[string]any)
			t, isText := m["text"].(string)
			if !ok || !isText {
				return compactJSON(v)
			}
			texts = append(texts, t)
		}
		return strings.Join(texts, "\n")
	}
	return compactJSON(v)
}

// compactJSON is v as JSON with sorted keys, so the same value always reads the same.
func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func placeholder(p *a2a.Part) string {
	what := "file"
	if p.MediaType != "" {
		what += " " + p.MediaType
	}
	if p.Filename != "" {
		what += " " + p.Filename
	}
	return "[" + what + "]"
}

// head is the start of s, at most n bytes, cut on a rune boundary.
func head(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	return cutRunes(s, n), true
}

// headTail is the start and the end of s, at most n bytes in all with the marker between them.
func headTail(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	marker := fmt.Sprintf("\n… %d bytes omitted …\n", len(s))
	keep := max(n-len(marker), 0)
	h := cutRunes(s, keep/2)
	t := s[len(s)-(keep-keep/2):]
	for len(t) > 0 && !utf8.RuneStart(t[0]) {
		t = t[1:]
	}
	omitted := len(s) - len(h) - len(t)
	return h + fmt.Sprintf("\n… %d bytes omitted …\n", omitted) + t, true
}

func cutRunes(s string, n int) string {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
