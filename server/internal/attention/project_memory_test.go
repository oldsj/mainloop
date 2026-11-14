package attention_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
)

// TestProjectMemoryProposalsOpenOneReviewItemPerProject: the sweep keeps one low-priority review
// item per project with memory proposals waiting, rewrites its count as proposals come and go,
// lists it after the other items, and resolves it once none is left, reopening the same item for a
// later proposal.
func TestProjectMemoryProposalsOpenOneReviewItemPerProject(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.exec(t, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES
		('p1', $1, 'o', 'r', 'o/r', 'https://x.example/o/r'),
		('p2', 'someone-else', 'o', 'x', 'o/x', 'https://x.example/o/x')`, owner)
	propose := func(id, project string) {
		t.Helper()
		e.exec(t, `INSERT INTO project_memory_entries (id, project_id, text, state, author) VALUES ($1, $2, $1, 'proposed', 'task')`, id, project)
	}
	e.exec(t, `INSERT INTO project_memory_entries (id, project_id, text, state, author, activated_seq, decided_at)
		VALUES ('m-active', 'p1', 'Already active.', 'active', 'main', 1, now())`)
	propose("m-other", "p2") // another owner's project: never this owner's item
	e.session(t, "s1", "active")
	e.hitl(t, "r1", "s1")
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

	if snap := sweep(); len(snap.Items) != 1 || snap.Count != 1 {
		t.Fatalf("active entries alone open nothing: %+v", snap.Items)
	}

	propose("m1", "p1")
	snap := sweep()
	if len(snap.Items) != 2 || snap.Count != 2 {
		t.Fatalf("items = %+v, count %d; want the HITL item and one review item, both counted", snap.Items, snap.Count)
	}
	// The review item opened after the HITL item but is listed after it: it is low priority.
	got := snap.Items[1]
	if snap.Items[0].Id != "hitl:r1" || got.Id != "memory:p1" || got.Kind != api.AttentionItemKindReview ||
		got.Title != "o/r" || got.Href != "/projects/p1" || got.TaskId != nil || len(got.SessionIds) != 0 ||
		got.Needed != "An agent proposed project memory. Accept or reject it." {
		t.Fatalf("items = %+v", snap.Items)
	}

	propose("m2", "p1")
	if snap := sweep(); snap.Items[1].Needed != "2 project memory proposals wait for you. Accept or reject them." {
		t.Fatalf("a second proposal rewrites the count: %+v", snap.Items[1])
	}
	var opened int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM attention_items WHERE source_kind = 'project_memory'`).Scan(&opened); err != nil || opened != 1 {
		t.Fatalf("project_memory rows = %d, %v; want one per project", opened, err)
	}
	before := len(e.inboxEvents(t))
	var updated, after time.Time
	if err := e.pool.QueryRow(ctx, `SELECT updated_at FROM attention_items WHERE id = 'attn_memory_p1'`).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		sweep()
	}
	if err := e.pool.QueryRow(ctx, `SELECT updated_at FROM attention_items WHERE id = 'attn_memory_p1'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !updated.Equal(after) || before != len(e.inboxEvents(t)) {
		t.Fatal("unchanged sweeps churned the item or its events")
	}

	e.exec(t, `UPDATE project_memory_entries SET state = 'rejected', decided_at = now() WHERE id IN ('m1', 'm2')`)
	if snap := sweep(); len(snap.Items) != 1 || snap.Items[0].Id != "hitl:r1" {
		t.Fatalf("no proposal left resolves the item: %+v", snap.Items)
	}

	e.exec(t, `UPDATE attention_items SET read_at = now() WHERE id = 'attn_memory_p1'`)
	propose("m3", "p1")
	snap = sweep()
	var read *time.Time
	if err := e.pool.QueryRow(ctx, `SELECT read_at FROM attention_items WHERE id = 'attn_memory_p1'`).Scan(&read); err != nil {
		t.Fatal(err)
	}
	if len(snap.Items) != 2 || snap.Items[1].Id != "memory:p1" || !strings.HasPrefix(snap.Items[1].Needed, "An agent proposed") || read != nil {
		t.Fatalf("a later proposal reopens the same item, unread: %+v, read_at %v", snap.Items, read)
	}
}
