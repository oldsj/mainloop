package attention_test

import (
	"context"
	"testing"
	"time"
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
		if snap.Count != want {
			t.Fatalf("count = %d, want %d: %+v", snap.Count, want, snap.Items)
		}
		if want > 0 && (snap.Items[0].Id != "reconciliation:t1" || snap.Items[0].ReasonCode == nil || string(*snap.Items[0].ReasonCode) != reason) {
			t.Fatalf("item %+v", snap.Items[0])
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
	// The task ends while its stop is still unresolved: the card stays. cancelOpenChildren can mark a
	// task cancelled before its attempt settles, and that attempt still holds a writer claim and
	// capacity until the stop observer confirms the stop or proves the runtime absent; the task being
	// final is not that confirmation. See TestUnresolvedStopSurvivesEndedSessionAndReplacement for the
	// live-task case of the same rule.
	e.exec(t, `UPDATE tasks SET status = 'cancelled', reason = NULL WHERE id = 't1'`)
	sweep(1, "runtime_stop_unresolved")
	unchanged(1, "runtime_stop_unresolved")
	// Only once the stop is confirmed does a final task's card drop.
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
		if err != nil || snap.Count != 1 {
			t.Fatalf("unresolved stop hidden: %+v %v", snap, err)
		}
	}
	e.exec(t, `UPDATE task_attempts SET stop_state = 'confirmed', evidence_refs = array_append(evidence_refs, 'runtime-stop-confirmed') WHERE task_id = 't1'`)
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := e.svc.Snapshot(ctx)
	if err != nil || snap.Count != 0 {
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

// A task that is already cancelled (or completed, or failed) still gets a card when its attempt
// carries unconfirmed runtime-stop evidence: cancelOpenChildren can mark a task final before its
// attempt settles, and that attempt still holds a writer claim and capacity until the stop observer
// confirms the stop or proves the runtime absent. Finality alone is not that confirmation, so the
// sweep must open (and keep open) the card until stop_state reaches 'confirmed'.
func TestReconciliationOpensForAFinalTaskWithAnUnresolvedStop(t *testing.T) {
	for _, status := range []string{"cancelled", "completed", "failed"} {
		t.Run(status, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			e.blockedTask(t, "t1", "", "r1")
			e.exec(t, `UPDATE tasks SET status = $1, reason = NULL WHERE id = 't1'`, status)
			e.exec(t, `UPDATE task_attempts SET state = 'draining', stop_state = 'unresolved', evidence_refs = ARRAY['runtime-stop-unresolved'] WHERE task_id = 't1'`)
			if err := e.svc.SyncTasks(ctx); err != nil {
				t.Fatal(err)
			}
			snap, err := e.svc.Snapshot(ctx)
			if err != nil || snap.Count != 1 || snap.Items[0].Id != "reconciliation:t1" {
				t.Fatalf("a %s task's unresolved stop must still reach the owner: %+v %v", status, snap, err)
			}
			e.exec(t, `UPDATE task_attempts SET stop_state = 'confirmed', evidence_refs = array_append(evidence_refs, 'runtime-stop-confirmed') WHERE task_id = 't1'`)
			if err := e.svc.SyncTasks(ctx); err != nil {
				t.Fatal(err)
			}
			snap, err = e.svc.Snapshot(ctx)
			if err != nil || snap.Count != 0 {
				t.Fatalf("a confirmed stop on a final task must drop the card: %+v %v", snap, err)
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
	if err != nil || snap.Count != 1 {
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
