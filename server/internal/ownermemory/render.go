package ownermemory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// Line is an entry as a session is given it: its text and the confirmation it had at the
// sequence it was rendered at.
type Line struct {
	ID          string
	Text        string
	ConfirmedAt time.Time
}

// Seq is the owner's current memory sequence, 0 before any entry was active.
func Seq(ctx context.Context, q *dbq.Queries, owner string) (int64, error) {
	st, err := q.GetOwnerMemorySettings(ctx, owner)
	if err != nil {
		return 0, fmt.Errorf("owner memory sequence: %w", err)
	}
	return st.Seq, nil
}

// At is the owner's active entries at seq, oldest first.
func At(ctx context.Context, q *dbq.Queries, owner string, seq int64) ([]Line, error) {
	if seq <= 0 {
		return nil, nil
	}
	rows, err := q.ListOwnerMemoryAt(ctx, dbq.ListOwnerMemoryAtParams{UserID: owner, Seq: seq})
	if err != nil {
		return nil, fmt.Errorf("owner memory at %d: %w", seq, err)
	}
	out := make([]Line, len(rows))
	for i, r := range rows {
		out[i] = Line{ID: r.ID, Text: r.Text, ConfirmedAt: r.ConfirmedAt}
	}
	return out, nil
}

// ForAttempt returns the entries an attempt's brief carries. The first call pins the owner's
// current sequence on the attempt; later calls, such as a resend of the brief, read the entries at
// that pinned sequence, so every send carries the same bytes.
func ForAttempt(ctx context.Context, q *dbq.Queries, attemptID, owner string) ([]Line, error) {
	seq, err := q.PinAttemptOwnerMemorySeq(ctx, dbq.PinAttemptOwnerMemorySeqParams{ID: attemptID, UserID: owner})
	if err != nil {
		return nil, fmt.Errorf("pin owner memory sequence: %w", err)
	}
	if seq == nil {
		return nil, nil
	}
	return At(ctx, q, owner, *seq)
}

// sectionIntro opens the section. It names the precedence: the assigned work, project instructions
// and project memory all win over owner memory.
const sectionIntro = "The owner's own standing preferences, across every project. Follow them unless the project's " +
	"instructions or memory below, or the assigned work, say otherwise. If one looks wrong or out of date, say so " +
	"(main: ask the owner); do not quietly ignore it.\n\n"

// Section renders entries as the "## Owner memory" section, or "" when there are none. Dates are
// absolute, so a resend of a pinned brief is the same bytes.
func Section(lines []Line) string {
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Owner memory\n" + sectionIntro)
	for _, l := range lines {
		b.WriteString("- " + l.render() + "\n")
	}
	return b.String()
}

func (l Line) render() string {
	return fmt.Sprintf("%s (owner memory %s, confirmed %s)", l.Text, l.ID, l.ConfirmedAt.UTC().Format(time.DateOnly))
}

// Changes renders what is active after and was not before, and the reverse, as the
// "## Owner memory changes" part of a running session's context block, or "" when the set of
// entries is the same (a confirmation alone changes only a date).
func Changes(before, after []Line) string {
	was := make(map[string]bool, len(before))
	for _, l := range before {
		was[l.ID] = true
	}
	is := make(map[string]bool, len(after))
	var b strings.Builder
	for _, l := range after {
		is[l.ID] = true
		if !was[l.ID] {
			b.WriteString("- New: " + l.render() + "\n")
		}
	}
	for _, l := range before {
		if !is[l.ID] {
			fmt.Fprintf(&b, "- Removed: %s (owner memory %s)\n", l.Text, l.ID)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "## Owner memory changes\n" + b.String()
}

// Update is the owner memory part of a session's next message. Changed is whether the session's
// owner memory moved since what it was given (nothing given counts as sequence 0); only then does
// the delivery record Seq, the owner's current sequence. Text is the whole section for a session
// that had none, else the changes since, and "" when nothing it reads changed.
type Update struct {
	Changed bool
	Seq     int64
	Text    string
}

// UpdateSince renders the owner memory part of the next message of a session that was given seq
// (given false: none at all).
func UpdateSince(ctx context.Context, q *dbq.Queries, owner string, given bool, seq int64) (Update, error) {
	if !given {
		seq = 0
	}
	cur, err := Seq(ctx, q, owner)
	if err != nil {
		return Update{}, err
	}
	if cur == seq {
		return Update{Seq: cur}, nil
	}
	out := Update{Changed: true, Seq: cur}
	after, err := At(ctx, q, owner, cur)
	if err != nil {
		return Update{}, err
	}
	if seq <= 0 {
		out.Text = Section(after)
		return out, nil
	}
	before, err := At(ctx, q, owner, seq)
	if err != nil {
		return Update{}, err
	}
	out.Text = Changes(before, after)
	return out, nil
}
