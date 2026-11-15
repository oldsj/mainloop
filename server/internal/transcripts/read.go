package transcripts

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/kagent"
)

// ErrNotFound is a session the owner does not have.
var ErrNotFound = errors.New("transcripts: session not found")

// Read bounds.
const (
	DefaultReadLimit = 200
	MaxReadLimit     = 500
	// liveSources, livePages and liveTasks bound the live tail: the newest active kagent Sessions, the
	// pages read from each one's cursor, and the newest uncopied tasks read whole.
	liveSources = 2
	livePages   = 2
	liveTasks   = 3
	// liveTimeout bounds every kagent read the tail makes together.
	liveTimeout = 5 * time.Second
	// liveUnreadable is what a reader is told when the tail fails; the error itself is logged.
	liveUnreadable = "kagent could not be read for the turns not yet copied"
)

// ReadRequest asks for one page of a session's transcript.
type ReadRequest struct {
	// Owner is whose session it must be; empty is the service's owner.
	Owner     string
	SessionID string
	// After is the last entry id the reader has; 0 starts from the beginning.
	After int64
	// Before, when set, ends the page just before this entry id instead.
	Before int64
	// Latest, instead, returns the last entries.
	Latest bool
	// Limit is the most stored entries returned: 1 to MaxReadLimit, 0 for the default.
	Limit int
	// Live asks for the live tail when the page reaches the last stored entry.
	Live bool
	// Kinds keeps only entries of these kinds, stored and live; empty keeps all.
	Kinds []Kind
}

// Read returns a page of a session's stored entries, the kagent Sessions they are copied from, and,
// once the page reaches the last stored entry, the live tail. kagent is read with no transaction open
// and within liveTimeout; a tail that cannot be read leaves the stored page whole and says why.
func (s *Service) Read(ctx context.Context, r ReadRequest) (api.SessionTranscript, error) {
	out := api.SessionTranscript{Entries: []api.TranscriptEntry{}, Sources: []api.TranscriptSource{}, Live: []api.TranscriptEntry{}}
	q := dbq.New(s.Pool)
	owned, err := q.TranscriptSessionOwned(ctx, dbq.TranscriptSessionOwnedParams{ID: r.SessionID, UserID: s.owner(r.Owner)})
	if err != nil {
		return out, err
	}
	if !owned {
		return out, ErrNotFound
	}
	limit := r.Limit
	if limit <= 0 {
		limit = DefaultReadLimit
	}
	limit = min(limit, MaxReadLimit)
	var kinds []string
	for _, k := range r.Kinds {
		kinds = append(kinds, string(k))
	}
	rows, err := page(ctx, q, r, kinds, limit, &out)
	if err != nil {
		return out, err
	}
	for _, e := range rows {
		id := e.ID
		out.Entries = append(out.Entries, api.TranscriptEntry{
			Id: &id, TaskId: e.TaskID, Pos: int(e.Pos), Kind: api.TranscriptEntryKind(e.Kind),
			ToolName: e.ToolName, ToolCallId: e.ToolCallID, IsError: e.IsError, Text: e.Text,
			Truncated: e.Truncated, OrigBytes: int(e.OrigBytes), At: e.At,
		})
	}
	sources, err := q.ListTranscriptSources(ctx, r.SessionID)
	if err != nil {
		return out, err
	}
	for _, src := range sources {
		out.Sources = append(out.Sources, api.TranscriptSource{
			State: api.TranscriptSourceState(src.State), Reason: src.Reason, ScannedAt: src.ScannedAt,
			StoredBytes: src.StoredBytes, CreatedAt: src.CreatedAt,
		})
	}
	if r.Live && !out.HasMore {
		live, err := s.liveTail(ctx, sources)
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			s.log().WarnContext(ctx, "transcripts: live tail", "session_id", r.SessionID, "error", err)
			msg := liveUnreadable
			out.LiveError = &msg
		}
		out.Live = keepKinds(live, r.Kinds)
	}
	return out, nil
}

// page reads one page of stored entries, oldest first, and sets out's HasEarlier and HasMore: whether
// entries lie beyond it on either side.
func page(ctx context.Context, q *dbq.Queries, r ReadRequest, kinds []string, limit int, out *api.SessionTranscript) ([]dbq.ListTranscriptEntriesRow, error) {
	n := int32(limit) //nolint:gosec // bounded by MaxReadLimit
	var rows []dbq.ListTranscriptEntriesRow
	// lo and hi bound the page; an empty page sits where it would have been.
	var lo, hi int64
	if r.Before > 0 || r.Latest {
		var before *int64
		if r.Before > 0 {
			before = &r.Before
		}
		back, err := q.ListTranscriptEntriesBefore(ctx, dbq.ListTranscriptEntriesBeforeParams{SessionID: r.SessionID, BeforeID: before, Kinds: kinds, MaxRows: n})
		if err != nil {
			return nil, err
		}
		for i := len(back) - 1; i >= 0; i-- {
			rows = append(rows, dbq.ListTranscriptEntriesRow(back[i]))
		}
		lo, hi = 0, math.MaxInt64
		if before != nil {
			lo, hi = *before, *before-1
		}
	} else {
		var err error
		rows, err = q.ListTranscriptEntries(ctx, dbq.ListTranscriptEntriesParams{SessionID: r.SessionID, AfterID: max(r.After, 0), Kinds: kinds, MaxRows: n})
		if err != nil {
			return nil, err
		}
		lo, hi = max(r.After, 0)+1, max(r.After, 0)
	}
	if len(rows) > 0 {
		lo, hi = rows[0].ID, rows[len(rows)-1].ID
	}
	beyond, err := q.TranscriptEntriesBeyond(ctx, dbq.TranscriptEntriesBeyondParams{SessionID: r.SessionID, Lo: lo, Hi: hi, Kinds: kinds})
	if err != nil {
		return nil, err
	}
	out.HasEarlier, out.HasMore = beyond.Earlier, beyond.Later
	return rows, nil
}

func keepKinds(entries []api.TranscriptEntry, kinds []Kind) []api.TranscriptEntry {
	if len(kinds) == 0 {
		return entries
	}
	kept := entries[:0]
	for _, e := range entries {
		for _, k := range kinds {
			if string(e.Kind) == string(k) {
				kept = append(kept, e)
				break
			}
		}
	}
	return kept
}

func (s *Service) owner(o string) string {
	if o != "" {
		return o
	}
	return s.OwnerID
}

// Entry returns one stored entry whole, if it belongs to one of sessionIDs and owner (empty: the
// service's owner) has it.
func (s *Service) Entry(ctx context.Context, owner string, id int64, sessionIDs []string) (api.TranscriptEntry, string, error) {
	q := dbq.New(s.Pool)
	e, err := q.GetTranscriptEntry(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !slices.Contains(sessionIDs, e.SessionID)) {
		return api.TranscriptEntry{}, "", ErrNotFound
	}
	if err != nil {
		return api.TranscriptEntry{}, "", err
	}
	owned, err := q.TranscriptSessionOwned(ctx, dbq.TranscriptSessionOwnedParams{ID: e.SessionID, UserID: s.owner(owner)})
	if err != nil {
		return api.TranscriptEntry{}, "", err
	}
	if !owned {
		return api.TranscriptEntry{}, "", ErrNotFound
	}
	eid := e.ID
	return api.TranscriptEntry{
		Id: &eid, TaskId: e.TaskID, Pos: int(e.Pos), Kind: api.TranscriptEntryKind(e.Kind),
		ToolName: e.ToolName, ToolCallId: e.ToolCallID, IsError: e.IsError, Text: e.Text,
		Truncated: e.Truncated, OrigBytes: int(e.OrigBytes), At: e.At,
	}, e.SessionID, nil
}

// liveTail reads the turns of the newest active sources that have no copy yet: running, waiting, or
// finished since the last pass. Each is normalized as a copy would be (redacted and capped) and not
// stored. sources come newest first.
func (s *Service) liveTail(ctx context.Context, sources []dbq.TranscriptSession) ([]api.TranscriptEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, liveTimeout)
	defer cancel()
	o := s.opts()
	q := dbq.New(s.Pool)
	type pending struct {
		agent kagent.AgentRef
		id    string
	}
	var tail []pending
	active := 0
	// Oldest first, so the tail reads in order.
	for i := len(sources) - 1; i >= 0; i-- {
		if sources[i].State != string(StateActive) {
			continue
		}
		if active++; active > liveSources {
			break
		}
		src := sources[i]
		agent := kagent.AgentRef{Namespace: src.AgentNamespace, Name: src.AgentName}
		token := ""
		if src.TaskCursor != nil {
			token = *src.TaskCursor
		}
		for range livePages {
			tasks, next, err := s.Kagent.ListTasksPage(ctx, agent, src.ContextID, token, o.PageSize)
			if err != nil {
				return nil, err
			}
			ids := make([]string, 0, len(tasks))
			for _, t := range tasks {
				if t != nil {
					ids = append(ids, string(t.ID))
				}
			}
			copied, err := q.CopiedTranscriptTasks(ctx, ids)
			if err != nil {
				return nil, err
			}
			done := make(map[string]bool, len(copied))
			for _, id := range copied {
				done[id] = true
			}
			for _, id := range ids {
				if !done[id] {
					tail = append(tail, pending{agent, id})
				}
			}
			if next == "" {
				break
			}
			token = next
		}
	}
	if len(tail) > liveTasks {
		tail = tail[len(tail)-liveTasks:]
	}
	out := []api.TranscriptEntry{}
	for _, p := range tail {
		task, err := s.Kagent.GetTask(ctx, p.agent, p.id)
		if err != nil {
			return out, err
		}
		for _, e := range Normalize(task, o.Limits) {
			out = append(out, liveEntry(task, e))
		}
	}
	if len(out) > MaxReadLimit {
		out = out[len(out)-MaxReadLimit:]
	}
	return out, nil
}

func liveEntry(task *a2a.Task, e Entry) api.TranscriptEntry {
	return api.TranscriptEntry{
		TaskId: string(task.ID), Pos: e.Pos, Kind: api.TranscriptEntryKind(e.Kind), ToolName: e.ToolName,
		ToolCallId: e.ToolCallID, IsError: e.IsError, Text: e.Text, Truncated: e.Truncated,
		OrigBytes: e.OrigBytes, At: e.At, Live: true,
	}
}

// Search bounds.
const (
	DefaultSearchLimit = 20
	MaxSearchLimit     = 100
	MaxQueryLen        = 200
)

// SearchRequest is a full-text query over the owner's stored entries.
type SearchRequest struct {
	// Owner is whose entries are searched; empty is the service's owner.
	Owner                string
	Query                string
	ProjectID, SessionID *string
	// SessionIDs, when not nil, limits the search to these sessions; empty finds nothing.
	SessionIDs []string
	Limit      int
}

// Search returns the owner's best-matching stored entries.
func (s *Service) Search(ctx context.Context, r SearchRequest) (api.TranscriptSearchResults, error) {
	out := api.TranscriptSearchResults{Hits: []api.TranscriptSearchHit{}}
	limit := r.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	limit = min(limit, MaxSearchLimit)
	if r.SessionIDs != nil && len(r.SessionIDs) == 0 {
		return out, nil
	}
	rows, err := dbq.New(s.Pool).SearchTranscripts(ctx, dbq.SearchTranscriptsParams{
		Query: r.Query, OwnerID: s.owner(r.Owner), SessionID: r.SessionID, ProjectID: r.ProjectID, SessionIds: r.SessionIDs,
		MaxResults: int32(limit), //nolint:gosec // bounded above
	})
	if err != nil {
		return out, err
	}
	for _, h := range rows {
		out.Hits = append(out.Hits, api.TranscriptSearchHit{
			EntryId: h.ID, SessionId: h.SessionID, SessionTitle: h.SessionTitle, TaskId: h.TaskID,
			Pos: int(h.Pos), Kind: api.TranscriptEntryKind(h.Kind), ToolName: h.ToolName, At: h.At,
			Rank: h.Rank, Snippet: Snippet(h.Snippet),
		})
	}
	return out, nil
}

// maxSnippetParts is the contract's bound on a snippet's parts.
const maxSnippetParts = 64

// Snippet splits a headline marked with U+0002 (start) and U+0003 (stop) into plain-text parts. A
// mark the text itself contained only moves a highlight; nothing is ever interpreted as markup.
func Snippet(marked string) []api.TranscriptSnippetPart {
	parts := []api.TranscriptSnippetPart{}
	match := false
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			parts = append(parts, api.TranscriptSnippetPart{Text: b.String(), Match: match})
			b.Reset()
		}
	}
	for _, r := range marked {
		switch r {
		case '\x02':
			flush()
			match = true
		case '\x03':
			flush()
			match = false
		default:
			b.WriteRune(r)
		}
	}
	flush()
	if len(parts) > maxSnippetParts {
		// Keep the bound; the rest joins the last part, unmarked.
		var rest strings.Builder
		for _, p := range parts[maxSnippetParts-1:] {
			rest.WriteString(p.Text)
		}
		parts = append(parts[:maxSnippetParts-1], api.TranscriptSnippetPart{Text: rest.String()})
	}
	return parts
}
