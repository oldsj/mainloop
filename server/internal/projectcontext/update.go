package projectcontext

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// MaxUpdateBytes bounds the block a running task session gets in front of its next message when its
// project's context changed (docs/specs/project-context.md#running-sessions). Past it, the block
// says to read the rest with project_context.
const MaxUpdateBytes = 2048

// smallInstructionsBytes is the size up to which changed instructions are sent whole rather than as
// the lines that changed.
const smallInstructionsBytes = 1024

// UpdateMarker opens the block, the way the main session's context block opens with its own marker.
const UpdateMarker = "[mainloop:project-context]"

const updateCut = "… more changed: call `project_context` for the current instructions and memory.\n"

// Update is the project's context now, and what changed since a session was given an earlier one.
type Update struct {
	InstructionsVersion int
	MemorySeq           int64
	// Block is "" when nothing a session reads changed (or nothing changed at all).
	Block string
}

// UpdateSince reads the project's current instructions version and memory sequence, and when either
// differs from what a session was given (version, seq), renders what changed: the instructions
// (whole when small, else the lines removed and added) and the memory entries added and removed.
func UpdateSince(ctx context.Context, q *dbq.Queries, projectID string, version int, seq int64) (Update, error) {
	cur, err := Current(ctx, q, projectID)
	if err != nil {
		return Update{}, err
	}
	curSeq, err := q.ProjectMemorySeq(ctx, projectID)
	if err != nil {
		return Update{}, fmt.Errorf("memory sequence: %w", err)
	}
	out := Update{InstructionsVersion: cur.Version, MemorySeq: curSeq}
	if cur.Version == version && curSeq == seq {
		return out, nil
	}
	var change Change
	if cur.Version != version {
		old := Instructions{ProjectID: projectID}
		if version > 0 {
			row, err := q.GetProjectInstructionVersion(ctx, dbq.GetProjectInstructionVersionParams{
				ProjectID: projectID, Version: int32(version), //nolint:gosec // versions stay far below 2^31
			})
			if err != nil {
				return Update{}, fmt.Errorf("get instructions v%d: %w", version, err)
			}
			old = fromRow(row)
		}
		change.Instructions = &[2]Instructions{old, cur}
	}
	if curSeq != seq {
		before, err := memoryAt(ctx, q, projectID, seq)
		if err != nil {
			return Update{}, err
		}
		after, err := memoryAt(ctx, q, projectID, curSeq)
		if err != nil {
			return Update{}, err
		}
		change.Added, change.Removed = entryChanges(before, after)
	}
	out.Block = change.Render()
	return out, nil
}

func memoryAt(ctx context.Context, q *dbq.Queries, projectID string, seq int64) ([]Entry, error) {
	if seq <= 0 {
		return nil, nil
	}
	rows, err := q.ListProjectMemoryAt(ctx, dbq.ListProjectMemoryAtParams{ProjectID: projectID, Seq: seq})
	if err != nil {
		return nil, fmt.Errorf("memory at %d: %w", seq, err)
	}
	out := make([]Entry, len(rows))
	for i, r := range rows {
		out[i] = entryOf(r)
	}
	return out, nil
}

// entryChanges is what is active after and was not before (added), and the reverse (removed), each
// in its own list's order.
func entryChanges(before, after []Entry) (added, removed []Entry) {
	was := make(map[string]bool, len(before))
	for _, e := range before {
		was[e.ID] = true
	}
	is := make(map[string]bool, len(after))
	for _, e := range after {
		is[e.ID] = true
		if !was[e.ID] {
			added = append(added, e)
		}
	}
	for _, e := range before {
		if !is[e.ID] {
			removed = append(removed, e)
		}
	}
	return added, removed
}

// Change is what changed in a project's context between what a session was given and now.
type Change struct {
	// Instructions is the version given and the current one, when they differ.
	Instructions *[2]Instructions
	Added        []Entry
	Removed      []Entry
}

// Render writes the change as the block a session gets in front of its next message, at most
// MaxUpdateBytes, or "" when there is nothing to say.
func (c Change) Render() string {
	var b strings.Builder
	if c.Instructions != nil {
		renderInstructions(&b, c.Instructions[0], c.Instructions[1])
	}
	if len(c.Added) > 0 || len(c.Removed) > 0 {
		b.WriteString("## Project memory changes\n")
		for _, e := range c.Added {
			fmt.Fprintf(&b, "- New: %s (memory %s)\n", e.Text, e.ID)
		}
		for _, e := range c.Removed {
			fmt.Fprintf(&b, "- Removed: %s (memory %s)\n", e.Text, e.ID)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	block := UpdateMarker + " This project's context changed since your brief. " +
		"Where they differ, this replaces what your brief said.\n" + b.String()
	return capBlock(block)
}

func renderInstructions(b *strings.Builder, old, cur Instructions) {
	was := "none"
	if old.Version > 0 && old.Text != "" {
		was = fmt.Sprintf("v%d", old.Version)
	}
	switch {
	case cur.Version == 0 || cur.Text == "":
		if was == "none" {
			return
		}
		fmt.Fprintf(b, "## Project instructions cleared (were %s)\nThe owner cleared the project instructions; none apply now.\n", was)
	case len(cur.Text) <= smallInstructionsBytes || was == "none":
		fmt.Fprintf(b, "## Project instructions (owner, v%d, were %s)\n%s\n", cur.Version, was, cur.Text)
	default:
		removed, added := lineChanges(old.Text, cur.Text)
		fmt.Fprintf(b, "## Project instructions (owner, v%d, were %s): changed lines\n", cur.Version, was)
		if len(removed) == 0 && len(added) == 0 {
			b.WriteString("Only blank lines, spacing or line order changed.\n")
		}
		for _, l := range removed {
			fmt.Fprintf(b, "- %s\n", l)
		}
		for _, l := range added {
			fmt.Fprintf(b, "+ %s\n", l)
		}
	}
}

// lineChanges compares the non-blank lines of two texts as multisets: the lines of old that new
// lacks, and the lines of new that old lacks, each in its own text's order.
func lineChanges(old, cur string) (removed, added []string) {
	o, n := lines(old), lines(cur)
	inNew := make(map[string]int, len(n))
	for _, l := range n {
		inNew[l]++
	}
	inOld := make(map[string]int, len(o))
	for _, l := range o {
		inOld[l]++
		if inNew[l] > 0 {
			inNew[l]--
		} else {
			removed = append(removed, l)
		}
	}
	for _, l := range n {
		if inOld[l] > 0 {
			inOld[l]--
		} else {
			added = append(added, l)
		}
	}
	return removed, added
}

func lines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimRight(l, " \t\r"); strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// capBlock cuts a block past MaxUpdateBytes at the last whole line that fits (or the last whole
// character, for one long line) and says where the rest is.
func capBlock(block string) string {
	if len(block) <= MaxUpdateBytes {
		return block
	}
	limit := MaxUpdateBytes - len(updateCut)
	cut := strings.LastIndexByte(block[:limit], '\n') + 1
	if cut <= len(UpdateMarker) {
		cut = limit - 1 // room for the line break before the note
		for cut > 0 && !utf8.RuneStart(block[cut]) {
			cut--
		}
		return block[:cut] + "\n" + updateCut
	}
	return block[:cut] + updateCut
}
