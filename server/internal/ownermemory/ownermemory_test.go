package ownermemory_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/ownermemory"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const owner = "owner-1"

func ptr(s string) *string { return &s }

var (
	mainSession = ownermemory.Recorder{Author: ownermemory.AuthorMain, Source: projectcontext.Source{SessionID: ptr("main-1"), DeliveryID: ptr("d-1")}}
	supSession  = ownermemory.Recorder{Author: ownermemory.AuthorTask, Source: projectcontext.Source{SessionID: ptr("sup-1"), TaskID: ptr("t-1"), AttemptID: ptr("a-1")}}
	otherSup    = ownermemory.Recorder{Author: ownermemory.AuthorTask, Source: projectcontext.Source{SessionID: ptr("sup-2"), TaskID: ptr("t-2"), AttemptID: ptr("a-2")}}
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newService(t *testing.T) (*ownermemory.Service, *pgxpool.Pool, *clock) {
	t.Helper()
	db := testdb.New(t)
	for _, p := range []struct{ id, user string }{{"p1", owner}, {"p2", "someone-else"}} {
		if _, err := db.Pool.Exec(context.Background(),
			`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ($1, $2, 'o', $1, 'o/' || $1, 'https://github.com/o/' || $1)`,
			p.id, p.user); err != nil {
			t.Fatal(err)
		}
	}
	c := &clock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	return &ownermemory.Service{Pool: db.Pool, OwnerID: owner, Now: c.now}, db.Pool, c
}

func mustRemember(t *testing.T, svc *ownermemory.Service, by ownermemory.Recorder, text, replaces string) ownermemory.Entry {
	t.Helper()
	e, err := svc.Remember(context.Background(), by, text, replaces)
	if err != nil {
		t.Fatalf("remember %q: %v", text, err)
	}
	return e
}

func TestMainIsActiveAndASupervisorProposes(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	e := mustRemember(t, svc, mainSession, "  I review UI\n on my phone. ", "")
	if e.State != ownermemory.StateActive || e.Text != "I review UI on my phone." || e.LastConfirmedAt == nil || *e.Source.DeliveryID != "d-1" {
		t.Fatalf("main = %+v", e)
	}
	if again := mustRemember(t, svc, supSession, "I review UI on my phone.", ""); again.ID != e.ID {
		t.Fatalf("a repeat answers the open entry, got %s", again.ID)
	}
	p := mustRemember(t, svc, supSession, "One PR per change.", "")
	if p.State != ownermemory.StateProposed || p.LastConfirmedAt != nil || *p.Source.TaskID != "t-1" {
		t.Fatalf("proposal = %+v", p)
	}
	if _, err := svc.Forget(ctx, otherSup, p.ID); !errors.Is(err, ownermemory.ErrNotYours) {
		t.Fatalf("another supervisor withdraws: %v", err)
	}
	if _, err := svc.Forget(ctx, supSession, e.ID); !errors.Is(err, ownermemory.ErrNotYours) {
		t.Fatalf("a supervisor forgets main's: %v", err)
	}
	if _, err := svc.Forget(ctx, mainSession, p.ID); !errors.Is(err, ownermemory.ErrEntryState) {
		t.Fatalf("main forgets a proposal: %v", err)
	}
	if _, err := svc.Remember(ctx, mainSession, "key ghp_"+strings.Repeat("a", 36), ""); !errors.Is(err, ownermemory.ErrSecret) {
		t.Fatalf("a secret: %v", err)
	}
	if w, err := svc.Forget(ctx, supSession, p.ID); err != nil || w.State != ownermemory.StateWithdrawn {
		t.Fatalf("withdraw = %+v, %v", w, err)
	}
	m, err := svc.Memory(ctx)
	if err != nil || len(m.Entries) != 1 || m.UsedBytes != len(e.Text) || m.ReviewAfterDays != ownermemory.DefaultReviewDays {
		t.Fatalf("memory = %+v, %v", m, err)
	}
}

func TestBoundsHold(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	if _, err := svc.Remember(ctx, mainSession, strings.Repeat("é", 251), ""); !errors.Is(err, projectcontext.ErrEntryTooLarge) {
		t.Fatalf("an entry over 500 bytes: %v", err)
	}
	for i := range 6 {
		mustRemember(t, svc, mainSession, strings.Repeat(string(rune('a'+i)), 500), "")
	}
	if _, err := svc.Remember(ctx, mainSession, strings.Repeat("z", 73), ""); !errors.Is(err, ownermemory.ErrMemoryFull) {
		t.Fatalf("past 3072 bytes: %v", err)
	}
	mustRemember(t, svc, mainSession, strings.Repeat("z", 72), "")

	for i := range ownermemory.MaxOpenProposals {
		mustRemember(t, svc, supSession, "proposal "+string(rune('a'+i)), "")
	}
	if _, err := svc.Remember(ctx, supSession, "one too many", ""); !errors.Is(err, ownermemory.ErrTooManyProposals) {
		t.Fatalf("an eleventh proposal: %v", err)
	}
}

// TestConcurrentAcceptsStayWithinTheBound: accepts race for the last bytes; the owner's lock lets
// only those that fit through.
func TestConcurrentAcceptsStayWithinTheBound(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	mustRemember(t, svc, mainSession, strings.Repeat("m", 500), "")
	var props []ownermemory.Entry
	for i := range 8 {
		props = append(props, mustRemember(t, svc, supSession, strings.Repeat(string(rune('a'+i)), 500), ""))
	}
	var wg sync.WaitGroup
	for _, p := range props {
		wg.Go(func() { _, _ = svc.Decide(ctx, p.ID, ownermemory.DecisionAccept, "") })
	}
	wg.Wait()
	m, err := svc.Memory(ctx)
	if err != nil || m.UsedBytes != 6*500 {
		t.Fatalf("after concurrent accepts: %d bytes, %v", m.UsedBytes, err)
	}
}

func TestReplacingAndPinnedRenders(t *testing.T) {
	svc, pool, c := newService(t)
	ctx := context.Background()
	q := dbq.New(pool)
	old := mustRemember(t, svc, mainSession, "Prefer tabs.", "")
	seq1, _ := ownermemory.Seq(ctx, q, owner)
	before, _ := ownermemory.At(ctx, q, owner, seq1)
	s1 := ownermemory.Section(before)
	if !strings.HasPrefix(s1, "## Owner memory\n") || !strings.HasSuffix(s1, "- Prefer tabs. (owner memory "+old.ID+", confirmed 2026-11-01)\n") {
		t.Fatalf("section = %q", s1)
	}

	p := mustRemember(t, svc, supSession, "Prefer spaces.", old.ID)
	c.add(48 * time.Hour)
	got, err := svc.Decide(ctx, p.ID, ownermemory.DecisionAccept, "")
	if err != nil || got.State != ownermemory.StateActive {
		t.Fatalf("accept = %+v, %v", got, err)
	}
	m, _ := svc.Memory(ctx)
	if len(m.Entries) != 1 || m.Entries[0].ID != p.ID {
		t.Fatalf("after replace = %+v", m.Entries)
	}
	// Keeping an entry changes its date, not the set: what was pinned before renders the same.
	c.add(24 * time.Hour)
	if _, err := svc.Decide(ctx, p.ID, ownermemory.DecisionKeep, ""); err != nil {
		t.Fatal(err)
	}
	again, _ := ownermemory.At(ctx, q, owner, seq1)
	if ownermemory.Section(again) != s1 {
		t.Fatalf("pinned render changed: %q", ownermemory.Section(again))
	}
	cur, _ := ownermemory.Seq(ctx, q, owner)
	now, _ := ownermemory.At(ctx, q, owner, cur)
	if len(now) != 1 || !strings.Contains(ownermemory.Section(now), "confirmed 2026-11-04") {
		t.Fatalf("current = %q", ownermemory.Section(now))
	}

	// A running session that had seq1 gets the change once; one with none gets the section.
	up, err := ownermemory.UpdateSince(ctx, q, owner, true, seq1, room)
	if err != nil || !up.Changed || up.Seq != cur || up.More ||
		up.Text != "## Owner memory changes\n- Removed: Prefer tabs. (owner memory "+old.ID+")\n- New: Prefer spaces. (owner memory "+p.ID+", confirmed 2026-11-04)\n" {
		t.Fatalf("update = %+v, %v", up, err)
	}
	if up, _ := ownermemory.UpdateSince(ctx, q, owner, true, cur, room); up.Changed || up.Text != "" {
		t.Fatalf("no change = %+v", up)
	}
	if up, _ := ownermemory.UpdateSince(ctx, q, owner, false, 0, room); !up.Changed || !strings.HasPrefix(up.Text, "## Owner memory\n") {
		t.Fatalf("none given = %+v", up)
	}
}

// room is a whole block's room for owner memory, with no project part.
var room = projectcontext.Room("")

// TestAnUpdateThatDoesNotFitRecordsOnlyWhatWasGiven: when the changes since what a session had do
// not fit the room left in its block, the update brings it to the newest sequence whose changes
// fit whole, removals first, and says more is left; with no step fitting it gives nothing, so the
// next message carries it all.
func TestAnUpdateThatDoesNotFitRecordsOnlyWhatWasGiven(t *testing.T) {
	svc, pool, _ := newService(t)
	ctx := context.Background()
	q := dbq.New(pool)
	gone := mustRemember(t, svc, mainSession, "Forget me: "+strings.Repeat("g", 400), "")
	given, _ := ownermemory.Seq(ctx, q, owner)
	if _, err := svc.Forget(ctx, mainSession, gone.ID); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		mustRemember(t, svc, mainSession, strings.Repeat(string(rune('a'+i)), 480), "")
	}
	cur, _ := ownermemory.Seq(ctx, q, owner)
	removed := "- Removed: " + gone.Text + " (owner memory " + gone.ID + ")\n"

	whole, err := ownermemory.UpdateSince(ctx, q, owner, true, given, 1<<20)
	if err != nil || !whole.Changed || whole.Seq != cur || whole.More || !strings.HasPrefix(whole.Text, "## Owner memory changes\n"+removed) {
		t.Fatalf("whole = %+v, %v", whole, err)
	}
	up, err := ownermemory.UpdateSince(ctx, q, owner, true, given, room)
	if err != nil || !up.Changed || !up.More || up.Seq <= given || up.Seq >= cur || len(up.Text) > room ||
		!strings.HasPrefix(up.Text, "## Owner memory changes\n"+removed) {
		t.Fatalf("cut = %+v, %v", up, err)
	}
	// What it left goes next time, from the sequence it recorded.
	if next, _ := ownermemory.UpdateSince(ctx, q, owner, true, up.Seq, room); !next.Changed || next.Seq <= up.Seq || strings.Contains(next.Text, "Removed") {
		t.Fatalf("next = %+v", next)
	}
	if none, _ := ownermemory.UpdateSince(ctx, q, owner, true, given, 100); none.Changed || !none.More || none.Seq != given || none.Text != "" {
		t.Fatalf("nothing fits = %+v", none)
	}
	block := projectcontext.Block(up.Text, up.More)
	if len(block) > projectcontext.MaxUpdateBytes || !strings.Contains(block, removed) || !strings.HasSuffix(block, "call `project_context` and `owner_memory` for the current context.\n") {
		t.Fatalf("block = %q", block)
	}
}

func TestEntriesFallDueAndLapse(t *testing.T) {
	svc, pool, c := newService(t)
	ctx := context.Background()
	q := dbq.New(pool)
	e := mustRemember(t, svc, mainSession, "I work late.", "")
	if days, err := svc.SetReviewAfterDays(ctx, 0); !errors.Is(err, ownermemory.ErrReviewDays) {
		t.Fatalf("0 days = %d, %v", days, err)
	}
	if days, err := svc.SetReviewAfterDays(ctx, 3); err != nil || days != 3 {
		t.Fatalf("3 days = %d, %v", days, err)
	}

	c.add(3*24*time.Hour - time.Minute)
	if due := e.DueAt(3); due == nil || !c.now().Before(*due) {
		t.Fatalf("due at %v, now %v", due, c.now())
	}
	c.add(time.Minute + 7*24*time.Hour - time.Minute)
	if ids, err := ownermemory.Lapse(ctx, q, owner, c.now()); err != nil || len(ids) != 0 {
		t.Fatalf("lapse inside the grace = %v, %v", ids, err)
	}
	c.add(time.Minute)
	if ids, err := ownermemory.Lapse(ctx, q, owner, c.now()); err != nil || len(ids) != 1 {
		t.Fatalf("lapse = %v, %v", ids, err)
	}
	seq, _ := ownermemory.Seq(ctx, q, owner)
	if lines, _ := ownermemory.At(ctx, q, owner, seq); len(lines) != 0 {
		t.Fatalf("a lapsed entry is still loaded: %+v", lines)
	}
	m, _ := svc.Memory(ctx)
	if len(m.Entries) != 1 || m.Entries[0].State != ownermemory.StateLapsed || m.Entries[0].LapsedAt == nil || m.UsedBytes != 0 {
		t.Fatalf("lapsed = %+v", m)
	}
	if _, err := svc.Forget(ctx, mainSession, e.ID); !errors.Is(err, ownermemory.ErrEntryState) {
		t.Fatalf("main forgets a lapsed entry: %v", err)
	}
	kept, err := svc.Decide(ctx, e.ID, ownermemory.DecisionKeep, "")
	if err != nil || kept.State != ownermemory.StateActive || !kept.LastConfirmedAt.Equal(c.now()) || kept.LapsedAt != nil {
		t.Fatalf("keep = %+v, %v", kept, err)
	}
	seq, _ = ownermemory.Seq(ctx, q, owner)
	if lines, _ := ownermemory.At(ctx, q, owner, seq); len(lines) != 1 {
		t.Fatalf("a kept entry is loaded again: %+v", lines)
	}
}

func TestMovingAnEntry(t *testing.T) {
	svc, pool, _ := newService(t)
	ctx := context.Background()
	e := mustRemember(t, svc, mainSession, "Use fictional data in demos.", "")
	if _, err := svc.Decide(ctx, e.ID, ownermemory.DecisionMoveToProject, "p2"); !errors.Is(err, ownermemory.ErrProjectNotFound) {
		t.Fatalf("another owner's project: %v", err)
	}
	moved, err := svc.Decide(ctx, e.ID, ownermemory.DecisionMoveToProject, "p1")
	if err != nil || moved.State != ownermemory.StateMoved || moved.MovedToProjectID == nil || *moved.MovedToProjectID != "p1" {
		t.Fatalf("move = %+v, %v", moved, err)
	}
	pm, err := (&projectcontext.Service{Pool: pool, OwnerID: owner}).Memory(ctx, "p1")
	if err != nil || len(pm.Entries) != 1 || pm.Entries[0].ID != *moved.MovedToEntryID ||
		pm.Entries[0].State != projectcontext.StateActive || pm.Entries[0].Source.OwnerEntryID == nil || *pm.Entries[0].Source.OwnerEntryID != e.ID {
		t.Fatalf("project memory = %+v, %v", pm, err)
	}
	if _, err := svc.Decide(ctx, e.ID, ownermemory.DecisionMoved, ""); !errors.Is(err, ownermemory.ErrEntryState) {
		t.Fatalf("a moved-to-project entry marked moved to dotfiles: %v", err)
	}

	d := mustRemember(t, svc, mainSession, "Answer in short sentences.", "")
	if err := ownermemory.CheckDotfiles(ctx, pool, owner, "abc123", []byte("# Me\n\n- answer in short\n  sentences.\n")); err != nil {
		t.Fatal(err)
	}
	m, _ := svc.Memory(ctx)
	if len(m.Entries) != 1 || m.Entries[0].InDotfilesCommit == nil || *m.Entries[0].InDotfilesCommit != "abc123" {
		t.Fatalf("dotfiles flag = %+v", m.Entries)
	}
	if err := ownermemory.CheckDotfiles(ctx, pool, owner, "def456", []byte("# Me\n")); err != nil {
		t.Fatal(err)
	}
	if m, _ := svc.Memory(ctx); m.Entries[0].InDotfilesCommit != nil {
		t.Fatalf("flag kept after the line left: %+v", m.Entries)
	}
	if got, err := svc.Decide(ctx, d.ID, ownermemory.DecisionMoved, ""); err != nil || got.State != ownermemory.StateMoved || got.MovedToProjectID != nil {
		t.Fatalf("moved to dotfiles = %+v, %v", got, err)
	}
	if m, _ := svc.Memory(ctx); len(m.Entries) != 0 {
		t.Fatalf("open after moves = %+v", m.Entries)
	}
}

// TestMovingOntoAProjectProposalAcceptsIt: a move whose text a project proposal already holds
// accepts that proposal as the moved entry instead of adding a second, active copy.
func TestMovingOntoAProjectProposalAcceptsIt(t *testing.T) {
	svc, pool, _ := newService(t)
	ctx := context.Background()
	pc := &projectcontext.Service{Pool: pool, OwnerID: owner}
	prop, err := pc.Remember(ctx, "p1", projectcontext.Recorder{Author: projectcontext.AuthorTask,
		Source: projectcontext.Source{SessionID: ptr("sup-1"), TaskID: ptr("t-1"), AttemptID: ptr("a-1")}}, "Use fictional data in demos.", "")
	if err != nil || prop.State != projectcontext.StateProposed {
		t.Fatalf("proposal = %+v, %v", prop, err)
	}
	e := mustRemember(t, svc, mainSession, "Use fictional data in demos.", "")
	moved, err := svc.Decide(ctx, e.ID, ownermemory.DecisionMoveToProject, "p1")
	if err != nil || moved.State != ownermemory.StateMoved || moved.MovedToEntryID == nil || *moved.MovedToEntryID != prop.ID {
		t.Fatalf("move = %+v, %v; want it to name proposal %s", moved, err, prop.ID)
	}
	pm, err := pc.Memory(ctx, "p1")
	if err != nil || len(pm.Entries) != 1 || pm.Entries[0].ID != prop.ID || pm.Entries[0].State != projectcontext.StateActive {
		t.Fatalf("project memory = %+v, %v; want the one proposal, now active", pm.Entries, err)
	}
}
