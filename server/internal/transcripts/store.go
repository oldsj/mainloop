package transcripts

import (
	"context"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// Copy is one task's entries and what its copy receipt records.
type Copy struct {
	KagentSessionID string
	SessionID       string
	TaskID          string
	TaskState       string
	// TaskAt is when kagent recorded the task's last status; nil when it did not say.
	TaskAt  *time.Time
	Entries []Entry
}

// Save writes a task's copy receipt and its entries with q, which the caller runs in one transaction
// so a receipt never stands without its entries. It reports false, and writes nothing, when the task
// was copied before. The kagent Session's transcript_sessions row must exist.
func Save(ctx context.Context, q *dbq.Queries, c Copy) (bool, error) {
	var bytes int64
	p := dbq.InsertTranscriptEntriesParams{TaskID: c.TaskID, SessionID: c.SessionID}
	for _, e := range c.Entries {
		bytes += int64(len(e.Text))
		p.Pos = append(p.Pos, int32(e.Pos))                   //nolint:gosec // an index into one task's entries
		p.OrigBytes = append(p.OrigBytes, int32(e.OrigBytes)) //nolint:gosec // bounded by kagent's 16 MiB message limit
		p.Kind = append(p.Kind, string(e.Kind))
		p.ToolName = append(p.ToolName, e.ToolName)
		p.ToolCallID = append(p.ToolCallID, e.ToolCallID)
		p.IsError = append(p.IsError, e.IsError)
		p.Text = append(p.Text, e.Text)
		p.Truncated = append(p.Truncated, e.Truncated)
		at := ""
		if e.At != nil {
			at = e.At.UTC().Format(time.RFC3339Nano)
		}
		p.At = append(p.At, at)
	}
	n, err := q.InsertTranscriptTask(ctx, dbq.InsertTranscriptTaskParams{
		TaskID: c.TaskID, KagentSessionID: c.KagentSessionID, SessionID: c.SessionID, TaskState: c.TaskState,
		Entries: int32(len(c.Entries)), Bytes: bytes, TaskAt: c.TaskAt, //nolint:gosec // one task's entries
	})
	if err != nil || n == 0 {
		return false, err
	}
	if len(c.Entries) > 0 {
		if err := q.InsertTranscriptEntries(ctx, p); err != nil {
			return false, err
		}
	}
	if err := q.AddTranscriptSessionBytes(ctx, dbq.AddTranscriptSessionBytesParams{Bytes: bytes, KagentSessionID: c.KagentSessionID}); err != nil {
		return false, err
	}
	return true, nil
}

// State is transcript_sessions.state.
type State string

// Transcript session states.
const (
	// StateActive follows the kagent Session.
	StateActive State = "active"
	// StateComplete is a kagent Session that is gone, with every turn it had copied.
	StateComplete State = "complete"
	// StateUnavailable is a kagent Session that became unreadable before every turn was copied.
	StateUnavailable State = "unavailable"
)
