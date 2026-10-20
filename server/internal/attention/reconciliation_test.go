package attention_test

import (
	"context"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
)

func TestReconciliationAttentionTransitionsAndIdempotency(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.blockedTask(t, "t1", "", "r1")
	e.exec(t, `UPDATE tasks SET reason = 'reconciliation' WHERE id = 't1'`)
	sweep := func(want int, reason string) {
		t.Helper()
		if err := e.svc.SyncTasks(ctx); err != nil {
			t.Fatal(err)
		}
		snap, err := e.svc.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// A reconciliation item is Mainloop's own recovery work, not an owner decision: it is
		// listed, but never counted as a Needs-you decision.
		if snap.Count != 0 {
			t.Fatalf("count = %d, want 0 (a reconciliation item is not an owner decision): %+v", snap.Count, snap.Items)
		}
		if want == 0 {
			if len(snap.Items) != 0 {
				t.Fatalf("items = %+v, want none", snap.Items)
			}
			return
		}
		if len(snap.Items) != want || snap.Items[0].Id != "reconciliation:t1" ||
			snap.Items[0].Kind != api.AttentionItemKindProblem ||
			snap.Items[0].ReasonCode == nil || string(*snap.Items[0].ReasonCode) != reason {
			t.Fatalf("items = %+v, want %d problem item with reason %q", snap.Items, want, reason)
		}
	}
	unchanged := func(want int, reason string) {
		t.Helper()
		before := len(e.inboxEvents(t))
		var updated time.Time
		if err := e.pool.QueryRow(ctx, `SELECT updated_at FROM attention_items WHERE source_kind = 'task_reconciliation'`).Scan(&updated); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			sweep(want, reason)
		}
		var after time.Time
		if err := e.pool.QueryRow(ctx, `SELECT updated_at FROM attention_items WHERE source_kind = 'task_reconciliation'`).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if !updated.Equal(after) || before != len(e.inboxEvents(t)) {
			t.Fatal("unchanged sweeps churned rows or events")
		}
	}
	sweep(1, "reconciliation")
	unchanged(1, "reconciliation")
	e.exec(t, `UPDATE tasks SET status = 'running', reason = NULL WHERE id = 't1'`)
	sweep(0, "")
	unchanged(0, "")
	e.exec(t, `UPDATE tasks SET status = 'blocked', reason = 'reconciliation' WHERE id = 't1'`)
	sweep(1, "reconciliation")
	e.exec(t, `UPDATE task_attempts SET state = 'draining', stop_state = 'unresolved', evidence_refs = ARRAY['runtime-stop-unresolved'] WHERE task_id = 't1'`)
	sweep(1, "runtime_stop_unresolved")
	unchanged(1, "runtime_stop_unresolved")
	// The task ends while its stop is still unresolved: the card goes. The attempt keeps its claim
	// and capacity until the stop resolver confirms the stop, but that is internal cleanup, not a
	// decision for the owner. See TestUnresolvedStopSurvivesEndedSessionAndReplacement for the
	// live-task case, where the task is still open and the card stays.
	e.exec(t, `UPDATE tasks SET status = 'cancelled', reason = NULL WHERE id = 't1'`)
	sweep(0, "")
	unchanged(0, "")
	// Confirming the stop afterwards changes nothing for the owner.
	e.exec(t, `UPDATE task_attempts SET stop_state = 'confirmed' WHERE task_id = 't1'`)
	sweep(0, "")
	unchanged(0, "")
	var count int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM attention_items WHERE source_kind = 'task_reconciliation'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows %d: %v", count, err)
	}
}

func TestUnresolvedStopSurvivesEndedSessionAndReplacement(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.session(t, "s1", "cancelled")
	e.blockedTask(t, "t1", "s1", "r1")
	e.exec(t, `UPDATE tasks SET status = 'running', reason = NULL, current_attempt_id = NULL WHERE id = 't1'`)
	e.exec(t, `UPDATE task_attempts SET state = 'draining', stop_state = 'unresolved', evidence_refs = ARRAY['runtime-stop-unresolved'], archived_at = now() WHERE task_id = 't1'`)
	for range 3 {
		if err := e.svc.SyncTasks(ctx); err != nil {
			t.Fatal(err)
		}
		snap, err := e.svc.Snapshot(ctx)
		if err != nil || len(snap.Items) != 1 || snap.Count != 0 {
			t.Fatalf("unresolved stop hidden: %+v %v", snap, err)
		}
	}
	e.exec(t, `UPDATE task_attempts SET stop_state = 'confirmed', evidence_refs = array_append(evidence_refs, 'runtime-stop-confirmed') WHERE task_id = 't1'`)
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := e.svc.Snapshot(ctx)
	if err != nil || len(snap.Items) != 0 || snap.Count != 0 {
		t.Fatalf("confirmed stop still visible: %+v %v", snap, err)
	}
}

// A task that is already cancelled (or completed, or failed) for a plain block never gets a card:
// the owner cannot act on a task that is already over and has nothing else outstanding. This is
// not opened in the first place, not just resolved once already open.
func TestReconciliationNeverOpensForAnAlreadyFinalTaskWithoutAnUnresolvedStop(t *testing.T) {
	for _, status := range []string{"cancelled", "completed", "failed"} {
		t.Run(status, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			e.blockedTask(t, "t1", "", "r1")
			e.exec(t, `UPDATE tasks SET status = $1, reason = NULL WHERE id = 't1'`, status)
			if err := e.svc.SyncTasks(ctx); err != nil {
				t.Fatal(err)
			}
			snap, err := e.svc.Snapshot(ctx)
			if err != nil || snap.Count != 0 {
				t.Fatalf("a %s task without an unresolved stop must not reach the owner: %+v %v", status, snap, err)
			}
			var count int
			if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM attention_items WHERE source_kind = 'task_reconciliation'`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("rows %d: %v", count, err)
			}
		})
	}
}

// A task that is already cancelled (or completed, or failed) gets no card while its attempt carries
// unconfirmed runtime-stop evidence, and a card opened earlier resolves. The attempt still holds a
// writer claim and capacity until the stop resolver confirms the stop, but the owner cannot act on
// that: the resolver ends the native session itself, so the stop needs no decision.
func TestReconciliationDoesNotReachTheOwnerForAFinalTaskWithAnUnresolvedStop(t *testing.T) {
	for _, status := range []string{"cancelled", "completed", "failed"} {
		t.Run(status, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			// The item is Mainloop's own recovery work, not an owner decision, so Snapshot.Count
			// never counts it; open() reports whether it is listed at all.
			open := func() int {
				t.Helper()
				if err := e.svc.SyncTasks(ctx); err != nil {
					t.Fatal(err)
				}
				snap, err := e.svc.Snapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if snap.Count != 0 {
					t.Fatalf("count = %d, want 0 (a reconciliation item is not an owner decision)", snap.Count)
				}
				return len(snap.Items)
			}
			e.blockedTask(t, "t1", "", "r1")
			e.exec(t, `UPDATE tasks SET reason = 'reconciliation' WHERE id = 't1'`)
			e.exec(t, `UPDATE task_attempts SET state = 'draining', stop_state = 'unresolved', evidence_refs = ARRAY['runtime-stop-unresolved'] WHERE task_id = 't1'`)
			if n := open(); n != 1 {
				t.Fatalf("an open task blocked on an unresolved stop must stay listed, items = %d", n)
			}
			// The owner cancels it (or it ends otherwise) while the stop is still unresolved: the card
			// that was open resolves, and stays resolved for as long as the stop does.
			e.exec(t, `UPDATE tasks SET status = $1, reason = NULL WHERE id = 't1'`, status)
			for range 3 {
				if n := open(); n != 0 {
					t.Fatalf("a %s task's unresolved stop must not stay listed, items = %d", status, n)
				}
			}
			var openRows int
			if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM attention_items WHERE source_kind = 'task_reconciliation' AND state = 'open'`).Scan(&openRows); err != nil || openRows != 0 {
				t.Fatalf("open task_reconciliation rows = %d: %v", openRows, err)
			}
		})
	}
}

func TestReconciliationBlockWithoutAttempt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.exec(t, `INSERT INTO tasks (id, owner_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status, reason)
        VALUES ('t1', $1, 't1', 'Recover provision', 'brief', 'coordination', 'p', 'explicit', 'blocked', 'reconciliation')`, owner)
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := e.svc.Snapshot(ctx)
	if err != nil || len(snap.Items) != 1 || snap.Count != 0 {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	e.exec(t, `UPDATE tasks SET status = 'failed', reason = NULL WHERE id = 't1'`)
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err = e.svc.Snapshot(ctx)
	if err != nil || snap.Count != 0 {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
}
