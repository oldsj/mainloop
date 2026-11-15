package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/transcripts"
)

// Transcript tool bounds (docs/specs/transcripts.md).
const (
	transcriptDefaultLimit = 50
	transcriptMaxLimit     = 200
	// transcriptPreview is the most text one entry shows, unless entry_id asks for it whole.
	transcriptPreview = 2 << 10
	// transcriptBudget bounds the entry text of one task_transcript result.
	transcriptBudget   = 64 << 10
	searchDefaultLimit = 10
	searchMaxLimit     = 50
)

// transcriptTools read stored session transcripts. Both are read-only, send nothing to any agent,
// and answer only within the caller's task authority: what task_get could read.
func (s *Server) transcriptTools() []Tool {
	id := stringSchema(1, 100)
	kinds := []any{}
	for _, k := range []transcripts.Kind{
		transcripts.KindUser, transcripts.KindAssistant, transcripts.KindThinking, transcripts.KindToolCall,
		transcripts.KindToolResult, transcripts.KindStatus, transcripts.KindAsk, transcripts.KindOther,
	} {
		kinds = append(kinds, string(k))
	}
	return []Tool{
		{
			Name: agentauth.ToolTaskTranscript,
			Description: "Read what a task's agent did: its messages, tool calls and tool results, oldest first, " +
				"redacted, from its latest attempt's session (or attempt_id's). Reading costs the agent no turn. " +
				"Entries show a 2 KiB preview; pass entry_id to read one entry whole. Page with after=next_after. " +
				"At the end, live entries are the turn still running, not yet stored.",
			InputSchema: objectSchema(map[string]any{
				"task_id":    id,
				"attempt_id": optional(id),
				"after":      optional(map[string]any{"type": "integer", "minimum": 0}),
				"limit":      optional(map[string]any{"type": "integer", "minimum": 1, "maximum": transcriptMaxLimit}),
				"kinds": optional(map[string]any{
					"type": "array", "maxItems": len(kinds), "items": map[string]any{"type": "string", "enum": kinds},
				}),
				"entry_id": optional(map[string]any{"type": "integer", "minimum": 1}),
			}, "task_id"),
			Handle: s.taskTranscript,
		},
		{
			Name: agentauth.ToolTranscriptSearch,
			Description: "Full-text search over the stored transcripts of the tasks you can read (web-search syntax: " +
				"quoted phrases, or, -word), best match first. Each hit names its task and entry; read around it " +
				"with task_transcript(task_id, after=entry_id-1).",
			InputSchema: objectSchema(map[string]any{
				"q":       stringSchema(1, transcripts.MaxQueryLen),
				"task_id": optional(id),
				"limit":   optional(map[string]any{"type": "integer", "minimum": 1, "maximum": searchMaxLimit}),
			}, "q"),
			Handle: s.transcriptSearch,
		},
	}
}

func (s *Server) transcriptsAvailable() error {
	if s.Transcripts == nil {
		return failure("unavailable", "transcripts are not available")
	}
	return nil
}

func (s *Server) taskTranscript(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct {
		TaskID    string   `json:"task_id"`
		AttemptID *string  `json:"attempt_id"`
		After     *int64   `json:"after"`
		Limit     *int     `json:"limit"`
		Kinds     []string `json:"kinds"`
		EntryID   *int64   `json:"entry_id"`
	}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	taskID, err := text(in.TaskID, 1, 100)
	if err != nil {
		return Result{}, err
	}
	req := transcripts.ReadRequest{Limit: transcriptDefaultLimit, Live: true}
	if in.After != nil {
		if *in.After < 0 {
			return Result{}, errValidation
		}
		req.After = *in.After
	}
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > transcriptMaxLimit {
			return Result{}, errValidation
		}
		req.Limit = *in.Limit
	}
	for _, k := range in.Kinds {
		if !validKind(k) {
			return Result{}, errValidation
		}
		req.Kinds = append(req.Kinds, transcripts.Kind(k))
	}
	if in.EntryID != nil && *in.EntryID < 1 {
		return Result{}, errValidation
	}
	if err := s.transcriptsAvailable(); err != nil {
		return Result{}, err
	}
	p, err := s.taskPrincipal(ctx, a)
	if err != nil {
		return Result{}, err
	}
	v, err := s.Tasks.Get(ctx, p, taskID)
	if err != nil {
		return Result{}, taskFailure(err)
	}
	var sessions []string
	var attempt *api.TaskAttempt
	for i := range v.Attempts {
		at := &v.Attempts[i]
		if at.SessionId == nil {
			continue
		}
		sessions = append(sessions, *at.SessionId)
		if in.AttemptID == nil || at.Id == *in.AttemptID {
			attempt = at // attempts come oldest first: the last one with a session wins
		}
	}
	if in.AttemptID != nil && attempt == nil {
		return Result{}, failure("404", "attempt not found, or it has no session")
	}

	if in.EntryID != nil {
		e, sessionID, err := s.Transcripts.Entry(ctx, p.OwnerID, *in.EntryID, sessions)
		if errors.Is(err, transcripts.ErrNotFound) {
			return Result{}, failure("404", "transcript entry not found in this task")
		}
		if err != nil {
			return Result{}, err
		}
		m := map[string]any{"task_id": taskID, "session_id": sessionID, "entry": transcriptEntryMap(e, false)}
		line := renderTranscriptEntry(e, e.Text)
		m["text"] = line
		return Result{Text: line, Structured: m}, nil
	}

	m := map[string]any{"task_id": taskID, "entries": []any{}, "live": []any{}, "has_more": false, "next_after": req.After}
	if attempt == nil {
		line := fmt.Sprintf("task %s has no attempt with a session yet: no transcript", taskID)
		m["text"] = line
		return Result{Text: line, Structured: m}, nil
	}
	req.Owner, req.SessionID = p.OwnerID, *attempt.SessionId
	tr, err := s.Transcripts.Read(ctx, req)
	if errors.Is(err, transcripts.ErrNotFound) {
		return Result{}, failure("404", "session not found")
	}
	if err != nil {
		return Result{}, err
	}

	var lines []string
	entries, live := []any{}, []any{}
	budget, next, more := transcriptBudget, req.After, tr.HasMore
	for _, e := range tr.Entries {
		shown, cut := preview(e.Text)
		if len(shown) > budget && len(entries) > 0 {
			more = true
			break
		}
		budget -= len(shown)
		entries = append(entries, transcriptEntryMap(e, cut))
		lines = append(lines, renderTranscriptEntry(e, shown))
		next = *e.Id
	}
	if !more {
		for _, e := range tr.Live {
			shown, cut := preview(e.Text)
			if len(shown) > budget {
				break
			}
			budget -= len(shown)
			live = append(live, transcriptEntryMap(e, cut))
			lines = append(lines, renderTranscriptEntry(e, shown))
		}
	}
	m["entries"], m["live"], m["has_more"], m["next_after"] = entries, live, more, next
	m["attempt_id"], m["session_id"] = attempt.Id, *attempt.SessionId
	if tr.LiveError != nil {
		m["live_error"] = *tr.LiveError
	}
	sources, err := structured(tr.Sources)
	if err == nil {
		m["sources"] = sources
	}
	head := fmt.Sprintf("task %s attempt %s: %d stored, %d live", taskID, attempt.Id, len(entries), len(live))
	if more {
		head += fmt.Sprintf("; more after=%d", next)
	}
	if tr.LiveError != nil {
		head += "; " + *tr.LiveError
	}
	line := strings.Join(append([]string{head}, lines...), "\n")
	m["text"] = line
	return Result{Text: line, Structured: m}, nil
}

func (s *Server) transcriptSearch(ctx context.Context, a agentauth.Actor, args json.RawMessage) (Result, error) {
	var in struct {
		Q      string  `json:"q"`
		TaskID *string `json:"task_id"`
		Limit  *int    `json:"limit"`
	}
	if err := decode(args, &in); err != nil {
		return Result{}, err
	}
	q, err := text(in.Q, 1, transcripts.MaxQueryLen)
	if err != nil {
		return Result{}, err
	}
	limit := searchDefaultLimit
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > searchMaxLimit {
			return Result{}, errValidation
		}
		limit = *in.Limit
	}
	if err := s.transcriptsAvailable(); err != nil {
		return Result{}, err
	}
	p, err := s.taskPrincipal(ctx, a)
	if err != nil {
		return Result{}, err
	}
	visible, err := s.Tasks.VisibleSessions(ctx, p)
	if err != nil {
		return Result{}, taskFailure(err)
	}
	sessions := []string{}
	for sid, tid := range visible {
		if in.TaskID == nil || tid == *in.TaskID {
			sessions = append(sessions, sid)
		}
	}
	sort.Strings(sessions)
	res, err := s.Transcripts.Search(ctx, transcripts.SearchRequest{Owner: p.OwnerID, Query: q, SessionIDs: sessions, Limit: limit})
	if err != nil {
		return Result{}, err
	}
	hits := []any{}
	lines := []string{fmt.Sprintf("%d hits for %q", len(res.Hits), q)}
	for _, h := range res.Hits {
		var b strings.Builder
		for _, part := range h.Snippet {
			if part.Match {
				b.WriteString("[[" + part.Text + "]]")
			} else {
				b.WriteString(part.Text)
			}
		}
		snippet := b.String()
		hits = append(hits, map[string]any{
			"task_id": visible[h.SessionId], "session_id": h.SessionId, "session_title": h.SessionTitle,
			"entry_id": h.EntryId, "turn_id": h.TaskId, "kind": string(h.Kind), "tool_name": h.ToolName,
			"at": h.At, "snippet": snippet,
		})
		label := string(h.Kind)
		if h.ToolName != "" {
			label += " " + h.ToolName
		}
		lines = append(lines, fmt.Sprintf("task %s #%d %s: %s", visible[h.SessionId], h.EntryId, label, oneLine(snippet)))
	}
	line := strings.Join(lines, "\n")
	return Result{Text: line, Structured: map[string]any{"hits": hits, "text": line}}, nil
}

func validKind(k string) bool {
	switch transcripts.Kind(k) {
	case transcripts.KindUser, transcripts.KindAssistant, transcripts.KindThinking, transcripts.KindToolCall,
		transcripts.KindToolResult, transcripts.KindStatus, transcripts.KindAsk, transcripts.KindOther:
		return true
	}
	return false
}

// preview cuts text to transcriptPreview bytes on a rune boundary, and reports whether it cut.
func preview(text string) (string, bool) {
	if len(text) <= transcriptPreview {
		return text, false
	}
	cut := transcriptPreview
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + " …", true
}

func transcriptEntryMap(e api.TranscriptEntry, cut bool) map[string]any {
	text, _ := preview(e.Text)
	if !cut {
		text = e.Text
	}
	return map[string]any{
		"id": e.Id, "turn_id": e.TaskId, "pos": e.Pos, "kind": string(e.Kind), "tool_name": e.ToolName,
		"tool_call_id": e.ToolCallId, "is_error": e.IsError, "text": text, "truncated": e.Truncated,
		"cut": cut, "orig_bytes": e.OrigBytes, "at": e.At, "live": e.Live,
	}
}

func renderTranscriptEntry(e api.TranscriptEntry, text string) string {
	label := string(e.Kind)
	if e.ToolName != "" {
		label += " " + e.ToolName
	}
	if e.IsError {
		label += " (error)"
	}
	ref := "live"
	if e.Id != nil {
		ref = fmt.Sprintf("#%d", *e.Id)
	}
	return fmt.Sprintf("%s %s: %s", ref, label, text)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
