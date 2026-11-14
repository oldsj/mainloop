package projectcontext_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/oldsj/mainloop/server/internal/projectcontext"
)

func ptr(s string) *string { return &s }

var (
	mainSession = projectcontext.Recorder{Author: projectcontext.AuthorMain, Source: projectcontext.Source{SessionID: ptr("main-1"), DeliveryID: ptr("d-1")}}
	taskSession = projectcontext.Recorder{Author: projectcontext.AuthorTask, Source: projectcontext.Source{SessionID: ptr("sup-1"), TaskID: ptr("t-1"), AttemptID: ptr("a-1")}}
	otherTask   = projectcontext.Recorder{Author: projectcontext.AuthorTask, Source: projectcontext.Source{SessionID: ptr("sup-2"), TaskID: ptr("t-2"), AttemptID: ptr("a-2")}}
)

func TestMainsEntriesAreActiveAndRepeatsAnswerTheSameEntry(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	e, err := svc.Remember(ctx, "p1", mainSession, "  The owner reviews\n  UI on a phone. ", "")
	if err != nil || e.State != projectcontext.StateActive || e.Author != projectcontext.AuthorMain ||
		e.Text != "The owner reviews UI on a phone." || e.DecidedAt == nil {
		t.Fatalf("remember = %+v, %v", e, err)
	}
	if *e.Source.SessionID != "main-1" || *e.Source.DeliveryID != "d-1" || e.Source.TaskID != nil {
		t.Fatalf("source = %+v", e.Source)
	}
	again, err := svc.Remember(ctx, "p1", mainSession, "The owner reviews UI on a phone.", "")
	if err != nil || again.ID != e.ID {
		t.Fatalf("repeat = %+v, %v; want entry %s", again, err, e.ID)
	}
	m, err := svc.Memory(ctx, "p1")
	if err != nil || len(m.Entries) != 1 || m.UsedBytes != len(e.Text) {
		t.Fatalf("memory = %+v, %v", m, err)
	}
}

func TestEntriesAreBoundedAndValidated(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	for _, c := range []struct {
		text string
		want error
	}{
		{"   ", projectcontext.ErrEmptyEntry},
		{strings.Repeat("é", 251), projectcontext.ErrEntryTooLarge},
		{"bad\x00byte", projectcontext.ErrInvalidText},
		{"bad \xff utf8", projectcontext.ErrInvalidText},
	} {
		if _, err := svc.Remember(ctx, "p1", mainSession, c.text, ""); !errors.Is(err, c.want) {
			t.Errorf("%q: %v, want %v", c.text, err, c.want)
		}
	}
	if _, err := svc.Remember(ctx, "p2", mainSession, "not my project", ""); !errors.Is(err, projectcontext.ErrNotFound) {
		t.Fatalf("other owner's project: %v", err)
	}

	// Eight 500-byte entries fill 4000 of 4096 bytes; a ninth is refused until one is replaced
	// by something shorter.
	var first projectcontext.Entry
	for i := range 8 {
		e, err := svc.Remember(ctx, "p1", mainSession, fmt.Sprintf("%d%s", i, strings.Repeat("x", 499)), "")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = e
		}
	}
	if _, err := svc.Remember(ctx, "p1", mainSession, strings.Repeat("y", 100), ""); !errors.Is(err, projectcontext.ErrMemoryFull) {
		t.Fatalf("past the bound: %v", err)
	}
	short, err := svc.Remember(ctx, "p1", mainSession, strings.Repeat("y", 100), first.ID)
	if err != nil || short.State != projectcontext.StateActive || short.ReplacesID == nil || *short.ReplacesID != first.ID {
		t.Fatalf("replace = %+v, %v", short, err)
	}
	m, _ := svc.Memory(ctx, "p1")
	if len(m.Entries) != 8 || m.UsedBytes != 7*500+100 {
		t.Fatalf("after replace: %d entries, %d bytes", len(m.Entries), m.UsedBytes)
	}
	// The replaced entry is ended and can be neither replaced nor forgotten again.
	if _, err := svc.Remember(ctx, "p1", mainSession, "another", first.ID); !errors.Is(err, projectcontext.ErrEntryState) {
		t.Fatalf("replace an ended entry: %v", err)
	}
}

func TestTaskEntriesAreProposalsOnlyTheOwnerDecides(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	old, err := svc.Remember(ctx, "p1", mainSession, "Deploys go through staging.", "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.Remember(ctx, "p1", taskSession, "Deploys go through staging, then a canary.", old.ID)
	if err != nil || p.State != projectcontext.StateProposed || p.Author != projectcontext.AuthorTask || p.DecidedAt != nil ||
		*p.Source.TaskID != "t-1" || *p.Source.AttemptID != "a-1" {
		t.Fatalf("proposal = %+v, %v", p, err)
	}
	m, _ := svc.Memory(ctx, "p1")
	if len(m.Entries) != 2 || m.Entries[0].ID != old.ID || m.Entries[1].ID != p.ID || m.UsedBytes != len(old.Text) {
		t.Fatalf("memory lists active first, proposals after, and counts only active bytes: %+v", m)
	}

	// Only the proposing session withdraws it; the main session cannot decide it either.
	if _, err := svc.Forget(ctx, "p1", otherTask, p.ID); !errors.Is(err, projectcontext.ErrNotYours) {
		t.Fatalf("another task withdraws: %v", err)
	}
	if _, err := svc.Forget(ctx, "p1", mainSession, p.ID); !errors.Is(err, projectcontext.ErrEntryState) {
		t.Fatalf("main forgets a proposal: %v", err)
	}
	// A task cannot end an active entry.
	if _, err := svc.Forget(ctx, "p1", taskSession, old.ID); !errors.Is(err, projectcontext.ErrNotYours) {
		t.Fatalf("task forgets main's entry: %v", err)
	}

	got, err := svc.Decide(ctx, "p1", p.ID, projectcontext.DecisionAccept)
	if err != nil || got.State != projectcontext.StateActive || got.DecidedAt == nil {
		t.Fatalf("accept = %+v, %v", got, err)
	}
	if again, err := svc.Decide(ctx, "p1", p.ID, projectcontext.DecisionAccept); err != nil || again.State != projectcontext.StateActive {
		t.Fatalf("accept again = %+v, %v", again, err)
	}
	m, _ = svc.Memory(ctx, "p1")
	if len(m.Entries) != 1 || m.Entries[0].ID != p.ID {
		t.Fatalf("accepting a replacement ends the entry it replaces: %+v", m)
	}
	if _, err := svc.Decide(ctx, "p1", p.ID, projectcontext.DecisionReject); !errors.Is(err, projectcontext.ErrEntryState) {
		t.Fatalf("reject an active entry: %v", err)
	}
	if r, err := svc.Decide(ctx, "p1", p.ID, projectcontext.DecisionRevoke); err != nil || r.State != projectcontext.StateForgotten {
		t.Fatalf("revoke = %+v, %v", r, err)
	}
	if r, err := svc.Decide(ctx, "p1", p.ID, projectcontext.DecisionRevoke); err != nil || r.State != projectcontext.StateForgotten {
		t.Fatalf("revoke again = %+v, %v", r, err)
	}

	q, err := svc.Remember(ctx, "p1", taskSession, "Ask before touching billing.", "")
	if err != nil {
		t.Fatal(err)
	}
	if r, err := svc.Decide(ctx, "p1", q.ID, projectcontext.DecisionReject); err != nil || r.State != projectcontext.StateRejected {
		t.Fatalf("reject = %+v, %v", r, err)
	}
	if _, err := svc.Decide(ctx, "p1", q.ID, projectcontext.DecisionAccept); !errors.Is(err, projectcontext.ErrEntryState) {
		t.Fatalf("accept a rejected proposal: %v", err)
	}
	w, err := svc.Remember(ctx, "p1", taskSession, "Something I got wrong.", "")
	if err != nil {
		t.Fatal(err)
	}
	if r, err := svc.Forget(ctx, "p1", taskSession, w.ID); err != nil || r.State != projectcontext.StateWithdrawn {
		t.Fatalf("withdraw = %+v, %v", r, err)
	}
	if _, err := svc.Decide(ctx, "p1", "nope", projectcontext.DecisionAccept); !errors.Is(err, projectcontext.ErrEntryNotFound) {
		t.Fatalf("missing entry: %v", err)
	}
}

func TestAcceptIsBoundedAndProposalsAreCapped(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	var props []projectcontext.Entry
	for i := range 20 {
		p, err := svc.Remember(ctx, "p1", taskSession, fmt.Sprintf("%02d%s", i, strings.Repeat("z", 498)), "")
		if err != nil {
			t.Fatal(err)
		}
		props = append(props, p)
	}
	if _, err := svc.Remember(ctx, "p1", taskSession, "one more", ""); !errors.Is(err, projectcontext.ErrTooManyProposals) {
		t.Fatalf("21st proposal: %v", err)
	}
	for i := range 8 {
		if _, err := svc.Decide(ctx, "p1", props[i].ID, projectcontext.DecisionAccept); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Decide(ctx, "p1", props[8].ID, projectcontext.DecisionAccept); !errors.Is(err, projectcontext.ErrMemoryFull) {
		t.Fatalf("accept past the bound: %v", err)
	}
	if e, _ := svc.Memory(ctx, "p1"); e.UsedBytes != 8*500 {
		t.Fatalf("a refused accept changed memory: %d bytes", e.UsedBytes)
	}
}

// TestConcurrentAcceptsStayWithinTheBound: accepts race under the project lock, so together they
// never take active memory past the bound.
func TestConcurrentAcceptsStayWithinTheBound(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	var props []projectcontext.Entry
	for i := range 12 {
		p, err := svc.Remember(ctx, "p1", taskSession, fmt.Sprintf("%02d%s", i, strings.Repeat("c", 498)), "")
		if err != nil {
			t.Fatal(err)
		}
		props = append(props, p)
	}
	var wg sync.WaitGroup
	for _, p := range props {
		wg.Go(func() { _, _ = svc.Decide(ctx, "p1", p.ID, projectcontext.DecisionAccept) })
	}
	wg.Wait()
	m, err := svc.Memory(ctx, "p1")
	if err != nil || m.UsedBytes > projectcontext.MaxMemoryBytes || m.UsedBytes != 8*500 {
		t.Fatalf("after concurrent accepts: %d bytes, %v", m.UsedBytes, err)
	}
}

func TestMemorySectionListsEntriesWithTheirIDs(t *testing.T) {
	if projectcontext.MemorySection(nil) != "" {
		t.Fatal("no entries render a section")
	}
	got := projectcontext.MemorySection([]projectcontext.Entry{{ID: "m1", Text: "One."}, {ID: "m2", Text: "Two."}})
	if !strings.HasPrefix(got, "## Project memory\n") || !strings.HasSuffix(got, "\n\n- One. (memory m1)\n- Two. (memory m2)\n") {
		t.Fatalf("section = %q", got)
	}
}
