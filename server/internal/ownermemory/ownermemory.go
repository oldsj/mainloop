// Package ownermemory is owner memory (docs/specs/owner-memory.md): project memory without a
// project. It holds the few lines of the owner's own standing preferences that are true across
// every project, and loads them into every session the owner has.
//
// It is deliberately small and aged on purpose. The main session's entries are active at once;
// a supervisor's are proposals the owner decides; workers and workspace agents cannot record any.
// An active entry not confirmed within the owner's review window is due for review, and one still
// not kept GraceDays later lapses: it stops being loaded until the owner keeps it.
//
// Every write takes the owner's settings row lock, so the active byte total and the owner's
// memory sequence move one writer at a time. Every change into or out of the active set, and every
// confirmation, takes the next sequence number; a brief pinned to a sequence renders the same
// entries, with the same dates, for ever.
package ownermemory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/dotfiles"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
)

// Bounds. Owner memory rides in every brief of every project, next to that project's
// instructions and memory, so it is smaller than project memory's 4096.
const (
	MaxEntryBytes    = projectcontext.MaxMemoryEntryBytes
	MaxBytes         = 3072
	MaxOpenProposals = 10
	// DefaultReviewDays is the review window of an owner who has not set one.
	DefaultReviewDays = 14
	MinReviewDays     = 1
	MaxReviewDays     = 365
	// GraceDays is how long a due entry stays loaded before it lapses. Fixed, not a setting.
	GraceDays = 7
)

// Typed failures the API and tool layers map to their own answers. Entry text failures are
// projectcontext's (ErrEmptyEntry, ErrEntryTooLarge, ErrInvalidText).
var (
	ErrEntryNotFound     = errors.New("ownermemory: entry not found")
	ErrEntryState        = errors.New("ownermemory: entry is not in a state that allows this")
	ErrMemoryFull        = errors.New("ownermemory: owner memory is full")
	ErrTooManyProposals  = errors.New("ownermemory: too many open proposals")
	ErrNotYours          = errors.New("ownermemory: entry was not proposed by this session")
	ErrSecret            = errors.New("ownermemory: entry looks like a secret")
	ErrProjectNotFound   = errors.New("ownermemory: project not found")
	ErrProjectMemoryFull = errors.New("ownermemory: the project's memory is full")
	ErrReviewDays        = errors.New("ownermemory: review_after_days is out of range")
	ErrNoProject         = errors.New("ownermemory: move_to_project needs a project")
)

// Author is who recorded an entry: main (active at once) or a supervisor task (a proposal).
type Author string

// Authors.
const (
	AuthorMain Author = "main"
	AuthorTask Author = "task"
)

// Entry states. proposed, active and lapsed are open; the rest are final.
const (
	StateProposed  = "proposed"
	StateActive    = "active"
	StateLapsed    = "lapsed"
	StateRejected  = "rejected"
	StateWithdrawn = "withdrawn"
	StateReplaced  = "replaced"
	StateForgotten = "forgotten"
	StateMoved     = "moved"
)

// Decision is the owner's decision on one entry.
type Decision string

// Decisions.
const (
	DecisionAccept        Decision = "accept"
	DecisionReject        Decision = "reject"
	DecisionKeep          Decision = "keep"
	DecisionForget        Decision = "forget"
	DecisionMoved         Decision = "moved"
	DecisionMoveToProject Decision = "move_to_project"
)

// Recorder is the agent recording or forgetting an entry, as the server resolved it.
type Recorder struct {
	Author Author
	Source projectcontext.Source
}

// Entry is one owner memory entry.
type Entry struct {
	ID               string
	Text             string
	State            string
	Author           Author
	Source           projectcontext.Source
	ReplacesID       *string
	CreatedAt        time.Time
	LastConfirmedAt  *time.Time
	DecidedAt        *time.Time
	LapsedAt         *time.Time
	InDotfilesCommit *string
	MovedToProjectID *string
	MovedToEntryID   *string
}

// DueAt is when an active or lapsed entry is due for review under a review window of days; nil
// for other states.
func (e Entry) DueAt(days int) *time.Time {
	if e.LastConfirmedAt == nil || (e.State != StateActive && e.State != StateLapsed) {
		return nil
	}
	t := e.LastConfirmedAt.AddDate(0, 0, days)
	return &t
}

// Memory is the owner's open entries (active, then lapsed, then proposals) and settings.
type Memory struct {
	Entries         []Entry
	UsedBytes       int
	ReviewAfterDays int
}

// Service reads and writes one owner's memory.
type Service struct {
	Pool    *pgxpool.Pool
	OwnerID string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC().Truncate(time.Microsecond)
	}
	return time.Now().UTC().Truncate(time.Microsecond)
}

// Memory reads the owner's open entries and review window.
func (s *Service) Memory(ctx context.Context) (Memory, error) {
	q := dbq.New(s.Pool)
	st, err := q.GetOwnerMemorySettings(ctx, s.OwnerID)
	if err != nil {
		return Memory{}, fmt.Errorf("owner memory settings: %w", err)
	}
	rows, err := q.ListOpenOwnerMemory(ctx, s.OwnerID)
	if err != nil {
		return Memory{}, fmt.Errorf("list owner memory: %w", err)
	}
	out := Memory{Entries: make([]Entry, len(rows)), ReviewAfterDays: int(st.ReviewAfterDays)}
	for i, r := range rows {
		out.Entries[i] = entryOf(r)
		if r.State == StateActive {
			out.UsedBytes += len(r.Text)
		}
	}
	return out, nil
}

// SetReviewAfterDays saves the owner's review window. It confirms nothing.
func (s *Service) SetReviewAfterDays(ctx context.Context, days int) (int, error) {
	if days < MinReviewDays || days > MaxReviewDays {
		return 0, ErrReviewDays
	}
	row, err := dbq.New(s.Pool).SetOwnerMemoryReviewDays(ctx, dbq.SetOwnerMemoryReviewDaysParams{
		UserID: s.OwnerID, ReviewAfterDays: int32(days), //nolint:gosec // checked above
	})
	if err != nil {
		return 0, err
	}
	return int(row.ReviewAfterDays), nil
}

// Remember records an entry. The main session's entry is active at once, within MaxBytes;
// a supervisor's is a proposal. replaces names an active entry the new one takes the place of when
// it becomes active. Recording text that is already open answers that entry; when main records
// it, the owner said it again, so an active one is confirmed and a lapsed one is active again.
func (s *Service) Remember(ctx context.Context, by Recorder, text, replaces string) (Entry, error) {
	text, err := projectcontext.FoldEntry(text)
	if err != nil {
		return Entry{}, err
	}
	if len(dotfiles.Scan([]dotfiles.File{{Path: "entry", Data: []byte(text)}})) > 0 {
		return Entry{}, ErrSecret
	}
	now := s.now()
	var out Entry
	err = s.inOwner(ctx, func(q *dbq.Queries, w *writer) error {
		if existing, err := q.FindOpenOwnerMemoryByText(ctx, dbq.FindOpenOwnerMemoryByTextParams{UserID: s.OwnerID, Text: text}); err == nil {
			if by.Author != AuthorMain {
				out = entryOf(existing)
				return nil
			}
			switch existing.State {
			case StateActive:
				out, err = w.confirm(ctx, existing, now)
			case StateLapsed:
				out, err = w.activate(ctx, existing, nil, now)
			default:
				out = entryOf(existing)
			}
			return err
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var old *dbq.OwnerMemoryEntry
		if replaces != "" {
			row, err := w.get(ctx, replaces)
			if err != nil {
				return err
			}
			if row.State != StateActive {
				return ErrEntryState
			}
			old = &row
		}
		params := dbq.InsertOwnerMemoryEntryParams{
			ID: uuid.NewString(), UserID: s.OwnerID, Text: text, Author: string(by.Author),
			SourceSessionID: by.Source.SessionID, SourceTaskID: by.Source.TaskID,
			SourceAttemptID: by.Source.AttemptID, SourceDeliveryID: by.Source.DeliveryID,
		}
		if old != nil {
			params.ReplacesID = &old.ID
		}
		if by.Author != AuthorMain {
			n, err := q.CountProposedOwnerMemory(ctx, s.OwnerID)
			if err != nil {
				return err
			}
			if n >= MaxOpenProposals {
				return ErrTooManyProposals
			}
			params.State = StateProposed
			row, err := q.InsertOwnerMemoryEntry(ctx, params)
			out = entryOf(row)
			return err
		}
		seq, err := w.makeRoom(ctx, len(text), old, now)
		if err != nil {
			return err
		}
		params.State, params.LastConfirmedAt = StateActive, &now
		row, err := q.InsertOwnerMemoryEntry(ctx, params)
		if err != nil {
			return err
		}
		out = entryOf(row)
		return q.StartOwnerMemorySpan(ctx, dbq.StartOwnerMemorySpanParams{EntryID: row.ID, UserID: s.OwnerID, Seq: seq, ConfirmedAt: now})
	})
	return out, err
}

// Forget ends an entry for an agent. The main session forgets an active entry; a supervisor
// withdraws a proposal it made itself. Forgetting an entry already ended that way answers it.
func (s *Service) Forget(ctx context.Context, by Recorder, entryID string) (Entry, error) {
	now := s.now()
	var out Entry
	err := s.inOwner(ctx, func(_ *dbq.Queries, w *writer) error {
		row, err := w.get(ctx, entryID)
		if err != nil {
			return err
		}
		if by.Author == AuthorMain {
			if row.State == StateLapsed {
				return ErrEntryState
			}
			out, err = w.end(ctx, row, StateForgotten, now, nil, nil)
			return err
		}
		if row.SourceSessionID == nil || by.Source.SessionID == nil || *row.SourceSessionID != *by.Source.SessionID {
			return ErrNotYours
		}
		out, err = w.closeProposal(ctx, row, StateWithdrawn)
		return err
	})
	return out, err
}

// Decide applies the owner's decision on one entry. projectID is the target of a move to a
// project. A decision that finds the entry already where it would leave it answers it again.
func (s *Service) Decide(ctx context.Context, entryID string, d Decision, projectID string) (Entry, error) {
	if d == DecisionMoveToProject && projectID == "" {
		return Entry{}, ErrNoProject
	}
	now := s.now()
	var out Entry
	err := s.inOwner(ctx, func(q *dbq.Queries, w *writer) error {
		row, err := w.get(ctx, entryID)
		if err != nil {
			return err
		}
		switch d {
		case DecisionAccept:
			out, err = w.accept(ctx, row, now)
		case DecisionReject:
			out, err = w.closeProposal(ctx, row, StateRejected)
		case DecisionKeep:
			switch row.State {
			case StateActive:
				out, err = w.confirm(ctx, row, now)
			case StateLapsed:
				out, err = w.activate(ctx, row, nil, now)
			default:
				err = ErrEntryState
			}
		case DecisionForget:
			out, err = w.end(ctx, row, StateForgotten, now, nil, nil)
		case DecisionMoved:
			if row.State == StateMoved && row.MovedToProjectID != nil {
				return ErrEntryState // moved to a project, not to the dotfiles
			}
			out, err = w.end(ctx, row, StateMoved, now, nil, nil)
		case DecisionMoveToProject:
			out, err = w.moveToProject(ctx, q, row, projectID, now)
		default:
			return fmt.Errorf("ownermemory: unknown decision %q", d)
		}
		return err
	})
	return out, err
}

// writer is one write under the owner's lock: the owner, and the settings row as locked.
type writer struct {
	q     *dbq.Queries
	owner string
	seq   int64
	// advanced is set once this write took a sequence number; later changes in it share that one.
	advanced bool
}

// inOwner runs fn under the owner's memory lock.
func (s *Service) inOwner(ctx context.Context, fn func(q *dbq.Queries, w *writer) error) error {
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		w, err := lock(ctx, q, s.OwnerID)
		if err != nil {
			return err
		}
		return fn(q, w)
	})
	if err == nil || known(err) {
		return err
	}
	return fmt.Errorf("owner memory: %w", err)
}

func lock(ctx context.Context, q *dbq.Queries, owner string) (*writer, error) {
	if err := q.EnsureOwnerMemorySettings(ctx, owner); err != nil {
		return nil, err
	}
	st, err := q.LockOwnerMemory(ctx, owner)
	if err != nil {
		return nil, err
	}
	return &writer{q: q, owner: owner, seq: st.Seq}, nil
}

func known(err error) bool {
	for _, k := range []error{
		ErrEntryNotFound, ErrEntryState, ErrMemoryFull, ErrTooManyProposals, ErrNotYours, ErrSecret,
		ErrProjectNotFound, ErrProjectMemoryFull, ErrReviewDays, ErrNoProject,
	} {
		if errors.Is(err, k) {
			return true
		}
	}
	return false
}

func (w *writer) get(ctx context.Context, id string) (dbq.OwnerMemoryEntry, error) {
	row, err := w.q.GetOwnerMemoryEntry(ctx, dbq.GetOwnerMemoryEntryParams{ID: id, UserID: w.owner})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrEntryNotFound
	}
	return row, err
}

// next is the sequence number this write's changes to the active set take.
func (w *writer) next(ctx context.Context) (int64, error) {
	if w.advanced {
		return w.seq, nil
	}
	w.seq++
	w.advanced = true
	return w.seq, w.q.SetOwnerMemorySeq(ctx, dbq.SetOwnerMemorySeqParams{UserID: w.owner, Seq: w.seq})
}

// makeRoom checks that n more active bytes fit, less the entry being replaced, then takes the next
// sequence and ends the replaced entry at it.
func (w *writer) makeRoom(ctx context.Context, n int, old *dbq.OwnerMemoryEntry, now time.Time) (int64, error) {
	used, err := w.q.ActiveOwnerMemoryBytes(ctx, w.owner)
	if err != nil {
		return 0, err
	}
	if old != nil {
		used -= int64(len(old.Text))
	}
	if used+int64(n) > MaxBytes {
		return 0, ErrMemoryFull
	}
	seq, err := w.next(ctx)
	if err != nil {
		return 0, err
	}
	if old != nil {
		if err := w.q.EndOwnerMemorySpan(ctx, dbq.EndOwnerMemorySpanParams{EntryID: old.ID, Seq: seq}); err != nil {
			return 0, err
		}
		if _, err := w.q.EndOwnerMemoryEntry(ctx, dbq.EndOwnerMemoryEntryParams{ID: old.ID, State: StateReplaced, At: now}); err != nil {
			return 0, err
		}
	}
	return seq, nil
}

// activate makes a proposal or a lapsed entry active, confirmed now, replacing old.
func (w *writer) activate(ctx context.Context, row dbq.OwnerMemoryEntry, old *dbq.OwnerMemoryEntry, now time.Time) (Entry, error) {
	seq, err := w.makeRoom(ctx, len(row.Text), old, now)
	if err != nil {
		return Entry{}, err
	}
	if err := w.q.StartOwnerMemorySpan(ctx, dbq.StartOwnerMemorySpanParams{EntryID: row.ID, UserID: w.owner, Seq: seq, ConfirmedAt: now}); err != nil {
		return Entry{}, err
	}
	got, err := w.q.ActivateOwnerMemoryEntry(ctx, dbq.ActivateOwnerMemoryEntryParams{ID: row.ID, ConfirmedAt: now})
	return entryOf(got), err
}

func (w *writer) accept(ctx context.Context, row dbq.OwnerMemoryEntry, now time.Time) (Entry, error) {
	switch row.State {
	case StateActive:
		return entryOf(row), nil
	case StateProposed:
	default:
		return Entry{}, ErrEntryState
	}
	// The entry it replaces counts only while it is still active; one ended since is left alone.
	var old *dbq.OwnerMemoryEntry
	if row.ReplacesID != nil {
		prev, err := w.get(ctx, *row.ReplacesID)
		if err != nil {
			return Entry{}, err
		}
		if prev.State == StateActive {
			old = &prev
		}
	}
	return w.activate(ctx, row, old, now)
}

// confirm moves an active entry's last confirmation to now. The date its rendering carries
// changes, so its span restarts at the next sequence.
func (w *writer) confirm(ctx context.Context, row dbq.OwnerMemoryEntry, now time.Time) (Entry, error) {
	seq, err := w.next(ctx)
	if err != nil {
		return Entry{}, err
	}
	if err := w.q.EndOwnerMemorySpan(ctx, dbq.EndOwnerMemorySpanParams{EntryID: row.ID, Seq: seq}); err != nil {
		return Entry{}, err
	}
	if err := w.q.StartOwnerMemorySpan(ctx, dbq.StartOwnerMemorySpanParams{EntryID: row.ID, UserID: w.owner, Seq: seq, ConfirmedAt: now}); err != nil {
		return Entry{}, err
	}
	got, err := w.q.ConfirmOwnerMemoryEntry(ctx, dbq.ConfirmOwnerMemoryEntryParams{ID: row.ID, ConfirmedAt: now})
	return entryOf(got), err
}

// end ends an active or lapsed entry as state (forgotten, moved or lapsed). An active one leaves the
// active set at the next sequence.
func (w *writer) end(ctx context.Context, row dbq.OwnerMemoryEntry, state string, now time.Time, project, entry *string) (Entry, error) {
	switch {
	case row.State == state:
		return entryOf(row), nil
	case row.State == StateActive:
		seq, err := w.next(ctx)
		if err != nil {
			return Entry{}, err
		}
		if err := w.q.EndOwnerMemorySpan(ctx, dbq.EndOwnerMemorySpanParams{EntryID: row.ID, Seq: seq}); err != nil {
			return Entry{}, err
		}
	case row.State == StateLapsed && state != StateLapsed:
	default:
		return Entry{}, ErrEntryState
	}
	got, err := w.q.EndOwnerMemoryEntry(ctx, dbq.EndOwnerMemoryEntryParams{
		ID: row.ID, State: state, At: now, MovedToProjectID: project, MovedToEntryID: entry,
	})
	return entryOf(got), err
}

func (w *writer) closeProposal(ctx context.Context, row dbq.OwnerMemoryEntry, state string) (Entry, error) {
	switch row.State {
	case state:
		return entryOf(row), nil
	case StateProposed:
	default:
		return Entry{}, ErrEntryState
	}
	got, err := w.q.CloseOwnerMemoryProposal(ctx, dbq.CloseOwnerMemoryProposalParams{ID: row.ID, State: state})
	return entryOf(got), err
}

// moveToProject adds the entry's text to the project's memory as an active entry and ends this one
// as moved, in one transaction under the owner's lock and then the project's.
func (w *writer) moveToProject(ctx context.Context, q *dbq.Queries, row dbq.OwnerMemoryEntry, projectID string, now time.Time) (Entry, error) {
	switch row.State {
	case StateMoved:
		if row.MovedToProjectID != nil && *row.MovedToProjectID == projectID {
			return entryOf(row), nil
		}
		return Entry{}, ErrEntryState
	case StateActive, StateLapsed:
	default:
		return Entry{}, ErrEntryState
	}
	if _, err := q.LockOwnedProject(ctx, dbq.LockOwnedProjectParams{ID: projectID, UserID: w.owner}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Entry{}, ErrProjectNotFound
		}
		return Entry{}, err
	}
	author := projectcontext.AuthorTask
	if Author(row.Author) == AuthorMain {
		author = projectcontext.AuthorMain
	}
	src := projectcontext.Source{
		SessionID: row.SourceSessionID, TaskID: row.SourceTaskID,
		AttemptID: row.SourceAttemptID, DeliveryID: row.SourceDeliveryID,
	}
	pe, err := projectcontext.AddMovedEntry(ctx, q, projectID, row.Text, author, src, row.ID)
	if errors.Is(err, projectcontext.ErrMemoryFull) {
		return Entry{}, ErrProjectMemoryFull
	}
	if err != nil {
		return Entry{}, err
	}
	return w.end(ctx, row, StateMoved, now, &projectID, &pe.ID)
}

// Lapse moves every active entry of the owner whose review window and grace have both run out by
// now to lapsed, each at a new sequence number, inside the caller's transaction. It is the attention
// sweep's first step, and returns the lapsed entries' ids.
func Lapse(ctx context.Context, q *dbq.Queries, owner string, now time.Time) ([]string, error) {
	params := dbq.ListLapsingOwnerMemoryParams{UserID: owner, GraceDays: GraceDays, Now: now}
	due, err := q.ListLapsingOwnerMemory(ctx, params)
	if err != nil || len(due) == 0 {
		return nil, err
	}
	w, err := lock(ctx, q, owner)
	if err != nil {
		return nil, err
	}
	// Read again under the lock: a keep may have landed in between.
	if due, err = q.ListLapsingOwnerMemory(ctx, params); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(due))
	for _, row := range due {
		// One sequence number per entry, so every step a running session is told of is small
		// enough to fit its context block whole (see UpdateSince).
		w.advanced = false
		if _, err := w.end(ctx, row, StateLapsed, now.UTC(), nil, nil); err != nil {
			return nil, err
		}
		ids = append(ids, row.ID)
	}
	return ids, nil
}

// CheckDotfiles flags every active and lapsed entry of the owner whose text appears in agentsMD,
// the AGENTS.md the owner's dotfiles exported at commit, and clears the flag of every other one.
// Both sides are folded as an entry is (one line, runs of whitespace as one space) and compared
// case-insensitively. The match is exact on purpose: a reworded line is not found.
func CheckDotfiles(ctx context.Context, pool *pgxpool.Pool, owner, commit string, agentsMD []byte) error {
	q := dbq.New(pool)
	rows, err := q.ListOwnerMemoryForDotfiles(ctx, owner)
	if err != nil || len(rows) == 0 {
		return err
	}
	file := fold(string(agentsMD))
	for _, r := range rows {
		var at *string
		if file != "" && strings.Contains(file, fold(r.Text)) {
			at = &commit
		}
		if err := q.SetOwnerMemoryInDotfiles(ctx, dbq.SetOwnerMemoryInDotfilesParams{ID: r.ID, Commit: at}); err != nil {
			return err
		}
	}
	return nil
}

func fold(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func entryOf(r dbq.OwnerMemoryEntry) Entry {
	return Entry{
		ID: r.ID, Text: r.Text, State: r.State, Author: Author(r.Author),
		Source: projectcontext.Source{
			SessionID: r.SourceSessionID, TaskID: r.SourceTaskID,
			AttemptID: r.SourceAttemptID, DeliveryID: r.SourceDeliveryID,
		},
		ReplacesID: r.ReplacesID, CreatedAt: r.CreatedAt, LastConfirmedAt: r.LastConfirmedAt,
		DecidedAt: r.DecidedAt, LapsedAt: r.LapsedAt, InDotfilesCommit: r.InDotfilesCommit,
		MovedToProjectID: r.MovedToProjectID, MovedToEntryID: r.MovedToEntryID,
	}
}
