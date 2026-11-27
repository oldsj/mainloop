package attention_test

import (
	"context"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
)

// TestOwnerMemoryOpensOneReviewItem: the sweep keeps one low-priority review item for the owner
// while an owner memory proposal waits or an entry is due, lapsed or already in the dotfiles,
// rewrites its line as counts change, lapses entries past the grace, and resolves the item once
// nothing is left.
func TestOwnerMemoryOpensOneReviewItem(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()
	e.svc.Now = func() time.Time { return now }
	sweep := func() api.AttentionSnapshot {
		t.Helper()
		if err := e.svc.SyncTasks(ctx); err != nil {
			t.Fatal(err)
		}
		snap, err := e.svc.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return snap
	}
	e.exec(t, `INSERT INTO owner_memory_settings (user_id, seq) VALUES ($1, 1)`, owner)
	e.exec(t, `INSERT INTO owner_memory_entries (id, user_id, text, state, author, last_confirmed_at, decided_at)
		VALUES ('o-fresh', $1, 'Fresh.', 'active', 'main', $2, $2)`, owner, now)
	e.exec(t, `INSERT INTO owner_memory_spans (entry_id, user_id, start_seq, confirmed_at) VALUES ('o-fresh', $1, 1, $2)`, owner, now)
	e.exec(t, `INSERT INTO owner_memory_entries (id, user_id, text, state, author) VALUES ('o-other', 'someone-else', 'x', 'proposed', 'task')`)
	if snap := sweep(); len(snap.Items) != 0 {
		t.Fatalf("a fresh entry opens nothing: %+v", snap.Items)
	}

	e.exec(t, `INSERT INTO owner_memory_entries (id, user_id, text, state, author) VALUES ('o-p1', $1, 'P1.', 'proposed', 'task'), ('o-p2', $1, 'P2.', 'proposed', 'task')`, owner)
	snap := sweep()
	if len(snap.Items) != 1 || snap.Count != 1 {
		t.Fatalf("items = %+v", snap.Items)
	}
	got := snap.Items[0]
	if got.Id != "owner-memory:"+owner || got.Kind != api.AttentionItemKindReview || got.Href != "/settings#memory" ||
		got.Title != "Owner memory" || got.Needed != "2 owner memory proposals." {
		t.Fatalf("item = %+v", got)
	}

	// Fifteen days on, the entry is due; seven more and it lapses, still to review.
	now = now.Add(15 * 24 * time.Hour)
	if got := sweep().Items[0].Needed; got != "2 owner memory proposals and 1 entry to review." {
		t.Fatalf("due = %q", got)
	}
	now = now.Add(7 * 24 * time.Hour)
	if got := sweep().Items[0].Needed; got != "2 owner memory proposals and 1 entry to review." {
		t.Fatalf("lapsed = %q", got)
	}
	var state string
	var seq int64
	if err := e.pool.QueryRow(ctx, `SELECT e.state, s.seq FROM owner_memory_entries e JOIN owner_memory_settings s ON s.user_id = e.user_id WHERE e.id = 'o-fresh'`).Scan(&state, &seq); err != nil || state != "lapsed" || seq != 2 {
		t.Fatalf("lapse = %s at %d, %v; want lapsed at a new sequence", state, seq, err)
	}

	e.exec(t, `UPDATE owner_memory_entries SET state = 'rejected', decided_at = now() WHERE id IN ('o-p1', 'o-p2')`)
	if got := sweep().Items[0].Needed; got != "1 owner memory entry to review." {
		t.Fatalf("lapsed alone = %q", got)
	}
	e.exec(t, `UPDATE owner_memory_entries SET state = 'forgotten', decided_at = now() WHERE id = 'o-fresh'`)
	if snap := sweep(); len(snap.Items) != 0 {
		t.Fatalf("nothing left resolves the item: %+v", snap.Items)
	}
}
