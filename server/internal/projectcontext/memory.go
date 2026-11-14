package projectcontext

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// Project memory bounds. Active entries go into every task brief of the project after the
// instructions, so their text together stays at half the instructions' bound.
const (
	MaxMemoryEntryBytes = 500
	MaxMemoryBytes      = 4096
	// maxOpenProposals bounds what one project's agents can queue for the owner.
	maxOpenProposals = 20
)

// Typed memory failures the API and tool layers map to their own answers.
var (
	ErrEntryNotFound    = errors.New("projectcontext: memory entry not found")
	ErrEntryState       = errors.New("projectcontext: memory entry is not in a state that allows this")
	ErrMemoryFull       = errors.New("projectcontext: project memory is full")
	ErrTooManyProposals = errors.New("projectcontext: too many open memory proposals")
	ErrNotYours         = errors.New("projectcontext: memory entry was not proposed by this session")
	ErrEmptyEntry       = errors.New("projectcontext: memory entry is empty")
	ErrEntryTooLarge    = errors.New("projectcontext: memory entry is too large")
)

// Author is who recorded an entry. Only the main session's entries are active from the start.
type Author string

const (
	AuthorMain  Author = "main"
	AuthorTask  Author = "task"
	AuthorAgent Author = "agent"
)

// Entry states.
const (
	StateProposed  = "proposed"
	StateActive    = "active"
	StateRejected  = "rejected"
	StateWithdrawn = "withdrawn"
	StateForgotten = "forgotten"
	StateReplaced  = "replaced"
)

// Source is where an entry came from, resolved by the server from the recording session.
type Source struct {
	SessionID  *string
	TaskID     *string
	AttemptID  *string
	DeliveryID *string
}

// Entry is one memory entry.
type Entry struct {
	ID         string
	ProjectID  string
	Text       string
	State      string
	Author     Author
	Source     Source
	ReplacesID *string
	CreatedAt  time.Time
	DecidedAt  *time.Time
}

// Memory is a project's open entries: active ones, then proposals.
type Memory struct {
	Entries   []Entry
	UsedBytes int
}

// Decision is the owner's decision on one entry.
type Decision string

const (
	DecisionAccept Decision = "accept"
	DecisionReject Decision = "reject"
	DecisionRevoke Decision = "revoke"
)

// Recorder is the agent recording or forgetting an entry.
type Recorder struct {
	Author Author
	Source Source
}

// Memory reads the project's open entries for its owner.
func (s *Service) Memory(ctx context.Context, projectID string) (Memory, error) {
	q := dbq.New(s.Pool)
	if _, err := q.GetOwnedProject(ctx, dbq.GetOwnedProjectParams{ID: projectID, UserID: s.OwnerID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Memory{}, ErrNotFound
		}
		return Memory{}, fmt.Errorf("get project: %w", err)
	}
	return OpenMemory(ctx, q, projectID)
}

// OpenMemory reads the project's open entries without an ownership check; callers have
// established the project already.
func OpenMemory(ctx context.Context, q *dbq.Queries, projectID string) (Memory, error) {
	rows, err := q.ListOpenProjectMemory(ctx, projectID)
	if err != nil {
		return Memory{}, fmt.Errorf("list memory: %w", err)
	}
	out := Memory{Entries: make([]Entry, len(rows))}
	for i, r := range rows {
		out.Entries[i] = entryOf(r)
		if r.State == StateActive {
			out.UsedBytes += len(r.Text)
		}
	}
	return out, nil
}

// Remember records an entry on the project. The main session's entry is active at once, within
// MaxMemoryBytes; anyone else's is a proposal for the owner. replaces names an active entry the
// new one takes the place of when it becomes active. Recording text that is already open answers
// that entry, so a retried call adds nothing.
func (s *Service) Remember(ctx context.Context, projectID string, by Recorder, text, replaces string) (Entry, error) {
	text, err := entryText(text)
	if err != nil {
		return Entry{}, err
	}
	var out Entry
	err = s.inProject(ctx, projectID, func(q *dbq.Queries) error {
		if existing, err := q.FindOpenProjectMemoryByText(ctx, dbq.FindOpenProjectMemoryByTextParams{ProjectID: projectID, Text: text}); err == nil {
			out = entryOf(existing)
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var old *dbq.ProjectMemoryEntry
		if replaces != "" {
			row, err := q.GetProjectMemoryEntry(ctx, dbq.GetProjectMemoryEntryParams{ID: replaces, ProjectID: projectID})
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrEntryNotFound
			}
			if err != nil {
				return err
			}
			if row.State != StateActive {
				return ErrEntryState
			}
			old = &row
		}
		params := dbq.InsertProjectMemoryEntryParams{
			ID: uuid.NewString(), ProjectID: projectID, Text: text, Author: string(by.Author),
			SourceSessionID: by.Source.SessionID, SourceTaskID: by.Source.TaskID,
			SourceAttemptID: by.Source.AttemptID, SourceDeliveryID: by.Source.DeliveryID,
		}
		if old != nil {
			params.ReplacesID = &old.ID
		}
		if by.Author != AuthorMain {
			n, err := q.CountProposedProjectMemory(ctx, projectID)
			if err != nil {
				return err
			}
			if n >= maxOpenProposals {
				return ErrTooManyProposals
			}
			params.State = StateProposed
			row, err := q.InsertProjectMemoryEntry(ctx, params)
			out = entryOf(row)
			return err
		}
		seq, err := makeRoom(ctx, q, projectID, len(text), old)
		if err != nil {
			return err
		}
		params.State, params.ActivatedSeq = StateActive, &seq
		row, err := q.InsertProjectMemoryEntry(ctx, params)
		out = entryOf(row)
		return err
	})
	return out, err
}

// Forget ends an entry for an agent. The main session ends an active entry; any other session
// withdraws a proposal it made itself. Forgetting an entry already ended that way answers it.
func (s *Service) Forget(ctx context.Context, projectID string, by Recorder, entryID string) (Entry, error) {
	var out Entry
	err := s.inProject(ctx, projectID, func(q *dbq.Queries) error {
		row, err := q.GetProjectMemoryEntry(ctx, dbq.GetProjectMemoryEntryParams{ID: entryID, ProjectID: projectID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrEntryNotFound
		}
		if err != nil {
			return err
		}
		if by.Author == AuthorMain {
			out, err = end(ctx, q, row, StateForgotten)
			return err
		}
		if row.SourceSessionID == nil || by.Source.SessionID == nil || *row.SourceSessionID != *by.Source.SessionID {
			return ErrNotYours
		}
		out, err = closeProposal(ctx, q, row, StateWithdrawn)
		return err
	})
	return out, err
}

// Decide applies the owner's decision on one entry. A decision that finds the entry already in
// the state it would leave it in answers the entry again.
func (s *Service) Decide(ctx context.Context, projectID, entryID string, d Decision) (Entry, error) {
	var out Entry
	err := s.inProject(ctx, projectID, func(q *dbq.Queries) error {
		row, err := q.GetProjectMemoryEntry(ctx, dbq.GetProjectMemoryEntryParams{ID: entryID, ProjectID: projectID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrEntryNotFound
		}
		if err != nil {
			return err
		}
		switch d {
		case DecisionReject:
			out, err = closeProposal(ctx, q, row, StateRejected)
		case DecisionRevoke:
			out, err = end(ctx, q, row, StateForgotten)
		case DecisionAccept:
			out, err = accept(ctx, q, row)
		default:
			return fmt.Errorf("projectcontext: unknown decision %q", d)
		}
		return err
	})
	return out, err
}

func accept(ctx context.Context, q *dbq.Queries, row dbq.ProjectMemoryEntry) (Entry, error) {
	switch row.State {
	case StateActive:
		return entryOf(row), nil
	case StateProposed:
	default:
		return Entry{}, ErrEntryState
	}
	// The entry it replaces counts only while it is still active; one ended since is left alone.
	var old *dbq.ProjectMemoryEntry
	if row.ReplacesID != nil {
		prev, err := q.GetProjectMemoryEntry(ctx, dbq.GetProjectMemoryEntryParams{ID: *row.ReplacesID, ProjectID: row.ProjectID})
		if err != nil {
			return Entry{}, err
		}
		if prev.State == StateActive {
			old = &prev
		}
	}
	seq, err := makeRoom(ctx, q, row.ProjectID, len(row.Text), old)
	if err != nil {
		return Entry{}, err
	}
	got, err := q.ActivateProjectMemoryEntry(ctx, dbq.ActivateProjectMemoryEntryParams{ID: row.ID, Seq: seq})
	return entryOf(got), err
}

// makeRoom checks that n more active bytes fit, less the entry being replaced, then takes the
// project's next memory sequence and ends the replaced entry at it.
func makeRoom(ctx context.Context, q *dbq.Queries, projectID string, n int, old *dbq.ProjectMemoryEntry) (int64, error) {
	used, err := q.ActiveProjectMemoryBytes(ctx, projectID)
	if err != nil {
		return 0, err
	}
	if old != nil {
		used -= int64(len(old.Text))
	}
	if used+int64(n) > MaxMemoryBytes {
		return 0, ErrMemoryFull
	}
	seq, err := q.ProjectMemorySeq(ctx, projectID)
	if err != nil {
		return 0, err
	}
	seq++
	if old != nil {
		if _, err := q.EndProjectMemoryEntry(ctx, dbq.EndProjectMemoryEntryParams{ID: old.ID, State: StateReplaced, Seq: seq}); err != nil {
			return 0, err
		}
	}
	return seq, nil
}

func end(ctx context.Context, q *dbq.Queries, row dbq.ProjectMemoryEntry, state string) (Entry, error) {
	switch row.State {
	case state:
		return entryOf(row), nil
	case StateActive:
	default:
		return Entry{}, ErrEntryState
	}
	seq, err := q.ProjectMemorySeq(ctx, row.ProjectID)
	if err != nil {
		return Entry{}, err
	}
	got, err := q.EndProjectMemoryEntry(ctx, dbq.EndProjectMemoryEntryParams{ID: row.ID, State: state, Seq: seq + 1})
	return entryOf(got), err
}

func closeProposal(ctx context.Context, q *dbq.Queries, row dbq.ProjectMemoryEntry, state string) (Entry, error) {
	switch row.State {
	case state:
		return entryOf(row), nil
	case StateProposed:
	default:
		return Entry{}, ErrEntryState
	}
	got, err := q.CloseProjectMemoryProposal(ctx, dbq.CloseProjectMemoryProposalParams{ID: row.ID, State: state})
	return entryOf(got), err
}

// inProject runs fn under the owner's project row lock, which serializes every memory write on
// the project.
func (s *Service) inProject(ctx context.Context, projectID string, fn func(q *dbq.Queries) error) error {
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if _, err := q.LockOwnedProject(ctx, dbq.LockOwnedProjectParams{ID: projectID, UserID: s.OwnerID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		return fn(q)
	})
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrEntryNotFound) || errors.Is(err, ErrEntryState) ||
		errors.Is(err, ErrMemoryFull) || errors.Is(err, ErrTooManyProposals) || errors.Is(err, ErrNotYours) {
		return err
	}
	return fmt.Errorf("project memory: %w", err)
}

// entryText validates an entry and folds it onto one line, so it renders as one list item.
func entryText(text string) (string, error) {
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return "", ErrInvalidText
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return "", ErrEmptyEntry
	}
	if len(text) > MaxMemoryEntryBytes {
		return "", ErrEntryTooLarge
	}
	return text, nil
}

// MemoryForAttempt returns the entries an attempt's brief carries. The first call pins the
// project's current memory sequence on the attempt; later calls, such as a resend of the brief,
// read the entries at that pinned sequence, so every send carries the same entries.
func MemoryForAttempt(ctx context.Context, q *dbq.Queries, attemptID, projectID string) ([]Entry, error) {
	seq, err := q.PinAttemptMemorySeq(ctx, dbq.PinAttemptMemorySeqParams{ID: attemptID, ProjectID: projectID})
	if err != nil {
		return nil, fmt.Errorf("pin memory sequence: %w", err)
	}
	if seq == nil || *seq == 0 {
		return nil, nil
	}
	rows, err := q.ListProjectMemoryAt(ctx, dbq.ListProjectMemoryAtParams{ProjectID: projectID, Seq: *seq})
	if err != nil {
		return nil, fmt.Errorf("list pinned memory: %w", err)
	}
	out := make([]Entry, len(rows))
	for i, r := range rows {
		out[i] = entryOf(r)
	}
	return out, nil
}

// MemorySection renders active entries as the brief section that carries them, or "" when there
// are none.
func MemorySection(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Project memory\n" +
		"Lessons recorded for every task in this project, from the owner's own words, facts the main session " +
		"verified, or proposals the owner accepted. Follow them like the instructions; if one is wrong or out of " +
		"date, say so in your report.\n\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "- %s (memory %s)\n", e.Text, e.ID)
	}
	return b.String()
}

func entryOf(r dbq.ProjectMemoryEntry) Entry {
	return Entry{
		ID: r.ID, ProjectID: r.ProjectID, Text: r.Text, State: r.State, Author: Author(r.Author),
		Source:     Source{SessionID: r.SourceSessionID, TaskID: r.SourceTaskID, AttemptID: r.SourceAttemptID, DeliveryID: r.SourceDeliveryID},
		ReplacesID: r.ReplacesID, CreatedAt: r.CreatedAt, DecidedAt: r.DecidedAt,
	}
}
