package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/health"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func intp(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

func (e *env) view(t testing.TB, f health.Filter) api.HealthView {
	t.Helper()
	if f.Hours == 0 {
		f.Hours = 24
	}
	v, err := e.svc.View(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Python: test_stream_snapshot_restart_and_attribution. The fixture's two tasks roll up to the
// numbers the Python backend produced: 12 observations, tokens 7/3, 3 failures, 1 repeated group.
func TestIngestFixtureRollsUp(t *testing.T) {
	e := newEnv(t)
	r1 := e.ingest(t, e.tasks[task1], nil)
	r2 := e.ingest(t, e.tasks[task2], nil)
	if r1.Stored != 9 || r2.Stored != 3 || len(r1.Reasons)+len(r2.Reasons) != 0 {
		t.Fatalf("stored %+v %+v", r1, r2)
	}
	if n := e.count(t, "health_events"); n != 12 {
		t.Fatalf("events = %d", n)
	}
	v := e.view(t, health.Filter{})
	if len(v.Rows) != 1 {
		t.Fatalf("rows = %d, want the one unassigned session row: %+v", len(v.Rows), v.Rows)
	}
	row := v.Rows[0]
	tot := row.Totals
	if row.Id != "unassigned:session-1" || row.Title == nil || *row.Title != "Unassigned" || row.TaskId != nil {
		t.Fatalf("row %+v", row)
	}
	if tot.Turns != 2 || tot.UsageObservedTurns != 2 || intp(tot.InputTokens) != 7 || intp(tot.OutputTokens) != 3 ||
		intp(tot.Failures) != 3 || intp(tot.RepeatedFailureGroups) != 1 {
		t.Fatalf("totals %+v", tot)
	}
	if len(row.Turns) != 2 || len(row.Sessions) != 1 || row.ExpectedReceipts != 2 || len(row.Gaps) != 0 {
		t.Fatalf("row %+v", row)
	}
	if v.Hours != 24 || v.Partial || v.Retention.EventsDays != 7 || v.Retention.SummariesDays != 90 || len(v.Unsupported) != 4 {
		t.Fatalf("view extras %+v", v)
	}
}

// Python: test_content_identity_conflicts_and_owner_api (the same task twice is a no-op).
func TestIngestIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.ingest(t, e.tasks[task1], nil)
	before := e.dump(t)
	res := e.ingest(t, e.tasks[task1], nil)
	if res.Stored != 0 || res.Turns != 0 || len(res.Reasons) != 0 {
		t.Fatalf("second ingest %+v", res)
	}
	if after := e.dump(t); after != before {
		t.Fatalf("state changed:\n%s\n%s", before, after)
	}
}

// mutate changes the data of the artifact at index i of a fresh copy of a fixture task.
func (e *env) mutated(t *testing.T, id string, i int, f func(map[string]any)) {
	t.Helper()
	f(e.tasks[id].Artifacts[i].Parts[0].Data().(map[string]any))
}

// Python: test_content_identity_conflicts_and_owner_api, test_lexical_paths_and_unknown_fields,
// test_business_artifacts_are_ignored_and_future_health_is_diagnostic.
func TestIngestRecordsGaps(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(e *env)
		task   string
		before bool // ingest the unmodified task first
		want   string
		stored int
	}{
		{name: "unknown field", mutate: func(e *env) {
			e.mutated(t, task2, 1, func(d map[string]any) { d["extra"] = "x" })
		}, want: health.GapInvalidEvent, stored: 2},
		{name: "unsafe path", task: task1, mutate: func(e *env) {
			e.mutated(t, task1, 1, func(d map[string]any) {
				if tool, ok := d["tool"].(map[string]any); ok {
					tool["path"] = "../secret.pem"
				}
			})
		}, want: health.GapInvalidEvent, stored: 8},
		{name: "oversize", mutate: func(e *env) {
			e.mutated(t, task2, 1, func(d map[string]any) { d["runtime_version"] = strings.Repeat("x", 9000) })
		}, want: health.GapOverflow, stored: 2},
		{name: "future timestamp", mutate: func(e *env) {
			e.mutated(t, task2, 1, func(d map[string]any) {
				d["occurred_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			})
		}, want: health.GapInvalidTimestamp, stored: 2},
		{name: "artifact id differs from event id", mutate: func(e *env) {
			e.tasks[task2].Artifacts[1].ID = "22222222-2222-4222-8222-222222222222:99"
		}, want: health.GapIdentityMismatch, stored: 2},
		{name: "event names another task", mutate: func(e *env) {
			e.mutated(t, task2, 1, func(d map[string]any) { d["a2a_task_id"] = "other" })
		}, want: health.GapIdentityMismatch, stored: 2},
		{name: "same id different content", before: true, mutate: func(e *env) {
			e.mutated(t, task2, 1, func(d map[string]any) { d["runtime_version"] = "9.9.9" })
		}, want: health.GapIdentityConflict, stored: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			if c.before {
				e.ingest(t, e.tasks[orStr(c.task, task2)], nil)
				e.tasks = fixtureTasks(t, e.at)
			}
			c.mutate(e)
			id := orStr(c.task, task2)
			res := e.ingest(t, e.tasks[id], nil)
			if len(res.Reasons) != 1 || res.Reasons[0] != c.want || res.Stored != c.stored {
				t.Fatalf("result %+v, want %s with %d stored", res, c.want, c.stored)
			}
			var reasons []string
			if err := e.pool.QueryRow(context.Background(), `SELECT reasons FROM health_collection WHERE state = 'collected'`).Scan(&reasons); err != nil {
				t.Fatal(err)
			}
			if len(reasons) != 1 || reasons[0] != c.want {
				t.Fatalf("receipt reasons %v", reasons)
			}
		})
	}
}

func TestIngestMismatchedSessionContext(t *testing.T) {
	e := newEnv(t)
	res := e.ingest(t, e.tasks[task2], func(in *health.Input) { in.ContextID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" })
	if res.Stored != 0 || len(res.Reasons) != 1 || res.Reasons[0] != health.GapIdentityMismatch {
		t.Fatalf("%+v", res)
	}
	if e.count(t, "health_events") != 0 {
		t.Fatal("stored events of another context")
	}
}

// Python: test_business_artifacts_are_ignored_and_future_health_is_diagnostic.
func TestIngestIgnoresOtherArtifacts(t *testing.T) {
	e := newEnv(t)
	task := e.tasks[task2]
	task.Artifacts = append(task.Artifacts, task.Artifacts[0])
	// A copy under a different name is not a health artifact.
	other := *task.Artifacts[0]
	other.Name = "business-result"
	task.Artifacts = append(task.Artifacts, &other)
	res := e.ingest(t, task, nil)
	if res.Stored != 3 || len(res.Reasons) != 0 {
		t.Fatalf("%+v", res)
	}
}

// Python: test_retention_freezes_totals_and_rejects_replay.
func TestRetentionBoundaries(t *testing.T) {
	e := newEnv(t)
	e.ingest(t, e.tasks[task1], nil)
	e.ingest(t, e.tasks[task2], nil)
	ctx := context.Background()
	now := time.Now()

	// Inside the window nothing moves.
	e.svc.Now = func() time.Time { return now.Add(6 * 24 * time.Hour) }
	before := e.dump(t)
	res, err := e.svc.Retain(ctx)
	if err != nil || res != (health.RetainResult{}) {
		t.Fatalf("inside the window: %+v %v", res, err)
	}
	if e.dump(t) != before {
		t.Fatal("retention changed rows inside their window")
	}

	// One event of task 2's turn goes past the window; the rest of both turns stay inside.
	e.exec(t, `UPDATE health_events SET occurred_at = $1 WHERE event_id = (
		SELECT event_id FROM health_events WHERE a2a_task_id = $2 ORDER BY sequence LIMIT 1)`, now.Add(-8*24*time.Hour), task2)
	e.svc.Now = func() time.Time { return now }
	totalsBefore := e.view(t, health.Filter{Hours: 24 * 30}).Rows[0].Totals
	res, err = e.svc.Retain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Frozen != 1 || res.EventsDeleted != 1 || res.TurnsDeleted != 0 {
		t.Fatalf("one expired event: %+v", res)
	}
	if n := e.count(t, "health_events"); n != 11 {
		t.Fatalf("events = %d, want 11", n)
	}
	var frozen int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM health_turns WHERE frozen AND a2a_task_id = $1`, task2).Scan(&frozen); err != nil || frozen != 1 {
		t.Fatalf("frozen = %d %v", frozen, err)
	}
	if after := e.view(t, health.Filter{Hours: 24 * 30}).Rows[0].Totals; !sameJSON(t, after, totalsBefore) {
		t.Fatalf("totals moved with the events:\n%+v\n%+v", totalsBefore, after)
	}

	// A frozen turn takes no more events, and an expired replay restores nothing.
	e.tasks = fixtureTasks(t, e.at)
	if res := e.ingest(t, e.tasks[task2], nil); res.Stored != 0 {
		t.Fatalf("replay into a frozen turn stored %d", res.Stored)
	}
	if n := e.count(t, "health_events"); n != 11 {
		t.Fatalf("events after replay = %d", n)
	}

	// Past the event window every event goes; the summaries stay until the longer window.
	e.svc.Now = func() time.Time { return now.Add(8 * 24 * time.Hour) }
	res, err = e.svc.Retain(ctx)
	if err != nil || res.EventsDeleted != 11 || res.TurnsDeleted != 0 {
		t.Fatalf("past the event window: %+v %v", res, err)
	}
	if e.count(t, "health_turns") != 2 || e.count(t, "health_events") != 0 {
		t.Fatalf("turns=%d events=%d", e.count(t, "health_turns"), e.count(t, "health_events"))
	}
	e.svc.Now = func() time.Time { return now.Add(100 * 24 * time.Hour) }
	res, err = e.svc.Retain(ctx)
	if err != nil || res.TurnsDeleted != 2 || res.ReceiptsPruned != 2 {
		t.Fatalf("past the summary window: %+v %v", res, err)
	}
	if e.dump(t) != `{"turns": [], "events": [], "collection": []}` {
		t.Fatalf("left over: %s", e.dump(t))
	}
}

// Python: test_collection_bounds_and_native_child_samples (the per-receipt bound).
func TestIngestBoundsObservationsPerTask(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, err := e.pool.Exec(ctx, `INSERT INTO health_events (owner_id, gateway, event_id, producer_epoch, sequence, session_id, runtime_session_id,
		a2a_task_id, turn_key, occurred_at, payload)
		SELECT $1, $2, 'fill:' || g, 'fill', g, 'session-1', $3, $4, 'fill', now(), '{}'::jsonb FROM generate_series(1, 2048) AS g`,
		owner, gateway, runtimeSession, task2)
	if err != nil {
		t.Fatal(err)
	}
	res := e.ingest(t, e.tasks[task2], nil)
	if res.Stored != 0 || len(res.Reasons) != 1 || res.Reasons[0] != health.GapOverflow {
		t.Fatalf("%+v", res)
	}
}

// Python: test_stream_snapshot_restart_and_attribution (recovery rebuilds the same rollups) and
// test_reconcile_sweep_runs_health_retention_recovery_and_cleanup.
func TestRecoveryRebuildsAfterLostCollection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.ingest(t, e.tasks[task1], nil)
	e.ingest(t, e.tasks[task2], nil)
	direct := e.dump(t)
	directView := e.view(t, health.Filter{})

	e.exec(t, `TRUNCATE health_events, health_turns, health_collection`)
	// The flag on a delivery says its receipt exists; a wiped collection leaves nothing to say it for.
	e.exec(t, `UPDATE deliveries SET health_tracked = false`)
	if err := e.svc.Recover(ctx, fakeAgents{}); err != nil {
		t.Fatal(err)
	}
	if got := e.dump(t); got != direct {
		t.Fatalf("recovered state differs:\n%s\n%s", direct, got)
	}
	if view := e.view(t, health.Filter{}); !sameJSON(t, view, directView) {
		t.Fatalf("recovered page differs:\n%+v\n%+v", directView, view)
	}
	if e.kc.callCount() != 2 {
		t.Fatalf("kagent reads = %d, want one per task", e.kc.callCount())
	}
	// Collected receipts are final: another pass reads nothing.
	if err := e.svc.Recover(ctx, fakeAgents{}); err != nil {
		t.Fatal(err)
	}
	if e.kc.callCount() != 2 {
		t.Fatalf("a collected receipt was read again: %d", e.kc.callCount())
	}
}

func sameJSON(t testing.TB, a, b any) bool {
	t.Helper()
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	return string(x) == string(y)
}

// Python: test_failed_ingestion_does_not_change_delivery, test_recovery_deadline_covers_collection.
func TestRecoveryBacksOffAndGivesUp(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.svc.MaxAttempts = 2
	e.kc.fail[task1] = errors.New("kagent is down")

	if err := e.svc.Recover(ctx, fakeAgents{}); err != nil {
		t.Fatal(err)
	}
	var state string
	var attempts int
	var reasons []string
	var wait time.Duration
	read := func() {
		t.Helper()
		err := e.pool.QueryRow(ctx, `SELECT state, attempts, reasons, next_attempt_at - now() FROM health_collection WHERE a2a_task_id = $1`, task1).
			Scan(&state, &attempts, &reasons, &wait)
		if err != nil {
			t.Fatal(err)
		}
	}
	read()
	if state != string(dbstate.HealthPending) || attempts != 1 || len(reasons) != 1 || reasons[0] != health.GapSourceUnavailable || wait < 10*time.Second {
		t.Fatalf("after one failure: %s %d %v %s", state, attempts, reasons, wait)
	}
	// The healthy task was collected anyway, and the failed one is not read again before its delay.
	if e.count(t, "health_turns") != 1 {
		t.Fatalf("turns = %d", e.count(t, "health_turns"))
	}
	calls := e.kc.callCount()
	if err := e.svc.Recover(ctx, fakeAgents{}); err != nil || e.kc.callCount() != calls {
		t.Fatalf("read again inside its backoff: %d -> %d %v", calls, e.kc.callCount(), err)
	}
	// The page says what it could not see.
	if gaps := e.view(t, health.Filter{}).Rows[0].Gaps; len(gaps) != 1 || gaps[0] != health.GapSourceUnavailable {
		t.Fatalf("gaps %v", gaps)
	}
	// The delay passes; the second failure is the last.
	e.exec(t, `UPDATE health_collection SET next_attempt_at = now() WHERE a2a_task_id = $1`, task1)
	if err := e.svc.Recover(ctx, fakeAgents{}); err != nil {
		t.Fatal(err)
	}
	read()
	if state != string(dbstate.HealthUnavailable) || attempts != 2 {
		t.Fatalf("after giving up: %s %d", state, attempts)
	}
	// A recovered kagent does not resurrect a receipt that was given up on.
	delete(e.kc.fail, task1)
	e.exec(t, `UPDATE health_collection SET next_attempt_at = now()`)
	calls = e.kc.callCount()
	if err := e.svc.Recover(ctx, fakeAgents{}); err != nil || e.kc.callCount() != calls {
		t.Fatalf("an unavailable receipt was read: %v", err)
	}
}

// Recovery drops the receipt of a session that was deleted, instead of retrying it forever.
func TestRecoveryAbandonsDeletedSession(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.svc.Recover(ctx, fakeAgents{}); err != nil { // discovers and collects both
		t.Fatal(err)
	}
	e.exec(t, `UPDATE health_collection SET state = 'pending'`)
	e.exec(t, `DELETE FROM sessions WHERE id = 'session-1'`)
	calls := e.kc.callCount()
	if err := e.svc.Recover(ctx, fakeAgents{}); err != nil {
		t.Fatal(err)
	}
	var unavailable int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM health_collection WHERE state = 'unavailable'`).Scan(&unavailable); err != nil || unavailable != 2 {
		t.Fatalf("unavailable = %d %v", unavailable, err)
	}
	if e.kc.callCount() != calls {
		t.Fatal("read a task of a deleted session")
	}
}

// Python: test_reconcile_sweep_runs_health_retention_recovery_and_cleanup, and the acceptance
// "sweeps are single-runner under a lease".
func TestSweepsAreSingleRunner(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	runner := reconcile.NewRunner(e.pool, e.svc.Log, nil)
	for _, sweep := range []reconcile.Sweep{e.svc.RecoverSweep(fakeAgents{}), e.svc.RetainSweep()} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO sweep_leases (name, holder, until) VALUES ($1, 'other-process', now() + interval '60 seconds')`, sweep.Name); err != nil {
			t.Fatalf("%s: take the lease: %v", sweep.Name, err)
		}
		reads := e.kc.callCount()
		ran, err := runner.RunSweep(ctx, sweep)
		if _, derr := e.pool.Exec(ctx, `DELETE FROM sweep_leases WHERE name = $1`, sweep.Name); derr != nil {
			t.Fatal(derr)
		}
		if err != nil || ran {
			t.Fatalf("%s ran while another process held its lease: %v %v", sweep.Name, ran, err)
		}
		if e.kc.callCount() != reads {
			t.Fatal("the held sweep read kagent")
		}
		ran, err = runner.RunSweep(ctx, sweep)
		if err != nil || !ran {
			t.Fatalf("%s did not run once free: %v %v", sweep.Name, ran, err)
		}
	}
	if e.count(t, "health_turns") != 2 {
		t.Fatalf("the recovery sweep collected %d turns", e.count(t, "health_turns"))
	}
	if sw := e.svc.RecoverSweep(fakeAgents{}); sw.Every <= 0 || e.svc.RetainSweep().Every <= 0 {
		t.Fatalf("a sweep has no period: %+v", sw)
	}
}

// Python: test_existing_reports_approval_and_owner_deletion, test_missing_usage_unassigned_claims_and_approval.
func TestViewFiltersAttributionAndOwnership(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.exec(t, `INSERT INTO tasks (id, owner_id, project_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status)
		VALUES ('task-1', $1, 'project-1', 'task-1', 'Add retry button', 'b', 'coordination', 'p', 'explicit', 'running')`, owner)
	e.exec(t, `INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider, configuration_revision,
		agent_namespace, agent_name, role, depth, state, session_id) VALUES ('attempt-1', 'task-1', 1, 'p', 'claude', 'r', 'ns', 'ag', 'supervisor', 1, 'active', 'session-1')`)
	e.exec(t, `INSERT INTO task_reports (id, task_id, attempt_id, request_id, request_digest, summary, outcome, owner_action, task_version)
		VALUES ('report-1', 'task-1', 'attempt-1', 'req', repeat('a', 64), 'Stuck', 'blocked', 'Pick', 1)`)
	e.exec(t, `INSERT INTO tasks (id, owner_id, project_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status)
		VALUES ('task-2', $1, 'project-1', 'task-2', 'Never started', 'b', 'coordination', 'p', 'explicit', 'queued')`, owner)
	e.exec(t, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('project-x', 'someone-else', 'o', 'x', 'o/x', 'https://x.example/o/x')`)

	e.ingest(t, e.tasks[task1], nil)
	e.ingest(t, e.tasks[task2], nil)

	v := e.view(t, health.Filter{})
	if len(v.Rows) != 2 {
		t.Fatalf("rows: %+v", v.Rows)
	}
	byID := map[string]api.HealthRow{}
	for _, r := range v.Rows {
		byID[r.Id] = r
	}
	row := byID["task-1"]
	if row.Title == nil || *row.Title != "Add retry button" || len(row.Reports) != 1 || row.Reports[0].Outcome != "blocked" ||
		len(row.Sessions) != 1 || row.Sessions[0].Title == nil || *row.Sessions[0].Title != "Fix the build" ||
		row.Sessions[0].TaskTitle == nil || *row.Sessions[0].TaskTitle != "Add retry button" ||
		row.Sessions[0].AttemptId == nil || *row.Sessions[0].AttemptId != "attempt-1" || row.Totals.Turns != 2 {
		t.Fatalf("task row: %+v", row)
	}
	idle := byID["task-2"]
	if len(idle.Sessions) != 1 || idle.Sessions[0].Provider != "unknown" || idle.Sessions[0].SessionId != nil ||
		idle.Sessions[0].Title != nil || idle.Sessions[0].TaskTitle == nil || idle.Totals.Turns != 0 ||
		idle.Totals.Coverage != api.HealthTotalsCoverageUnknown || idle.Totals.Failures != nil || idle.Totals.InputTokens != nil {
		t.Fatalf("task without a session: %+v", idle)
	}

	// Each filter narrows; session, attempt and provider filters hide tasks that have no session.
	for name, f := range map[string]health.Filter{
		"project": {ProjectID: ptr("project-1")}, "task": {TaskID: ptr("task-1")}, "session": {SessionID: ptr("session-1")},
		"attempt": {AttemptID: ptr("attempt-1")}, "provider": {Provider: ptr("claude")},
	} {
		got := e.view(t, f)
		want := 1
		if name == "project" {
			want = 2
		}
		if len(got.Rows) != want || got.Rows[0].Id != "task-1" {
			t.Errorf("%s filter: %d rows, first %q", name, len(got.Rows), got.Rows[0].Id)
		}
	}
	if got := e.view(t, health.Filter{Provider: ptr("codex")}); len(got.Rows) != 0 {
		t.Fatalf("provider codex: %+v", got.Rows)
	}

	// Another owner's records are not found, and so is an unknown id.
	for name, f := range map[string]health.Filter{
		"project": {ProjectID: ptr("project-x")}, "task": {TaskID: ptr("nope")}, "session": {SessionID: ptr("nope")},
		"attempt": {AttemptID: ptr("nope")},
	} {
		f.Hours = 24
		if _, err := e.svc.View(ctx, f); !errors.Is(err, health.ErrNotFound) {
			t.Errorf("%s: %v, want not found", name, err)
		}
	}
	if _, err := e.svc.Events(ctx, "nope", 24); !errors.Is(err, health.ErrNotFound) {
		t.Errorf("events of an unknown session: %v", err)
	}

	// The time window drops turns outside it.
	e.exec(t, `UPDATE health_turns SET occurred_at = now() - interval '3 days'`)
	if got := e.view(t, health.Filter{Hours: 24}); got.Rows[0].Totals.Turns != 0 && len(got.Rows) > 0 && got.Rows[0].Id == "task-1" {
		t.Fatalf("a turn outside the window counted: %+v", got.Rows[0].Totals)
	}
	if got := e.view(t, health.Filter{Hours: 24 * 7}); byIDTurns(got, "task-1") != 2 {
		t.Fatalf("a wider window lost the turns")
	}
}

func byIDTurns(v api.HealthView, id string) int {
	for _, r := range v.Rows {
		if r.Id == id {
			return r.Totals.Turns
		}
	}
	return 0
}

// Python: test_old_receipt_gaps_stay_out_of_the_selected_window.
func TestOldReceiptGapsStayOutOfTheWindow(t *testing.T) {
	e := newEnv(t)
	e.kc.fail[task1] = errors.New("down")
	e.svc.Now = nil
	if err := e.svc.Recover(context.Background(), fakeAgents{}); err != nil {
		t.Fatal(err)
	}
	if gaps := e.view(t, health.Filter{Hours: 24}).Rows[0].Gaps; len(gaps) != 1 {
		t.Fatalf("gaps = %v", gaps)
	}
	e.exec(t, `UPDATE deliveries SET created_at = now() - interval '3 days'`)
	e.exec(t, `UPDATE sessions SET created_at = now() - interval '3 days'`)
	e.exec(t, `UPDATE health_collection SET receipt_at = now() - interval '3 days'`)
	e.exec(t, `UPDATE health_turns SET occurred_at = now() - interval '3 days'`)
	if v := e.view(t, health.Filter{Hours: 24}); len(v.Rows) != 0 {
		t.Fatalf("an old receipt's gap is in the day window: %+v", v.Rows)
	}
}

func TestEventsRoute(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.ingest(t, e.tasks[task1], nil)
	e.ingest(t, e.tasks[task2], nil)
	out, err := e.svc.Events(ctx, "session-1", 24)
	if err != nil || out.Partial || len(out.Events) != 12 {
		t.Fatalf("%d events partial=%v %v", len(out.Events), out.Partial, err)
	}
	first := out.Events[0]
	if first.EventId == "" || first.TaskId != nil || first.AttemptId != nil || first.ReceivedAt.IsZero() {
		t.Fatalf("%+v", first)
	}
	// More than 200 observations come back as 200 and partial.
	_, err = e.pool.Exec(ctx, `INSERT INTO health_events (owner_id, gateway, event_id, producer_epoch, sequence, session_id, runtime_session_id,
		a2a_task_id, turn_key, occurred_at, payload)
		SELECT $1, $2, 'fill:' || g, 'fill', g, 'session-1', $3, $4, 'fill', now(), payload FROM generate_series(1, 250) AS g,
		LATERAL (SELECT payload FROM health_events LIMIT 1) AS p`, owner, gateway, runtimeSession, task2)
	if err != nil {
		t.Fatal(err)
	}
	out, err = e.svc.Events(ctx, "session-1", 24)
	if err != nil || !out.Partial || len(out.Events) != 200 {
		t.Fatalf("%d events partial=%v %v", len(out.Events), out.Partial, err)
	}
}

// The acceptance "GETs never write", twice over: the page, the events and the smoke ledger change
// no row and never touch kagent, and the transaction they read in is refused a write by Postgres.
func TestReadsNeverWrite(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.svc.Recover(ctx, fakeAgents{}); err != nil {
		t.Fatal(err)
	}
	e.kc.fail[task1] = errors.New("down") // a read that tried to recover would call kagent
	calls := e.kc.callCount()
	e.exec(t, `UPDATE health_collection SET state = 'pending' WHERE a2a_task_id = $1`, task1)
	before := e.dumpAll(t)
	if _, err := e.svc.View(ctx, health.Filter{Hours: 24}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Events(ctx, "session-1", 24); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Smoke(ctx, "project-1", ptr("main")); err != nil {
		t.Fatal(err)
	}
	if e.dumpAll(t) != before || e.kc.callCount() != calls {
		t.Fatal("a read wrote or called kagent")
	}
	err := health.ReadOnly(e.svc, ctx, func(q *dbq.Queries) error {
		_, err := q.FreezeHealthTurns(ctx, dbq.FreezeHealthTurnsParams{OwnerID: owner, Cutoff: time.Now()})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "read-only transaction") {
		t.Fatalf("a write in the read transaction: %v", err)
	}
}

func TestSmokeObservations(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.Smoke(ctx, "nope", nil); !errors.Is(err, health.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	e.exec(t, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('project-x', 'someone-else', 'o', 'x', 'o/x', 'https://x.example/o/x')`)
	if _, err := e.svc.Smoke(ctx, "project-x", nil); !errors.Is(err, health.ErrNotFound) {
		t.Fatalf("foreign project: %v", err)
	}

	out, err := e.svc.Smoke(ctx, "project-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.DeliveriesBusy || len(out.Deliveries) != 0 || len(out.CapacityHolders) != 0 || !out.GlobalCapacityAvailable ||
		out.ParentCapacity != 3 || out.PushConfirmed || len(out.Pushes) != 0 {
		t.Fatalf("idle: %+v", out)
	}

	// Open deliveries show; a session of another project does not.
	e.exec(t, `UPDATE deliveries SET state = 'sending' WHERE id = 'msg-1'`)
	out, _ = e.svc.Smoke(ctx, "project-1", nil)
	if !out.DeliveriesBusy || len(out.Deliveries) != 1 || out.Deliveries[0].MessageId != "msg-1" || out.Deliveries[0].State != "sending" {
		t.Fatalf("busy: %+v", out)
	}

	// Capacity.
	e.exec(t, `INSERT INTO tasks (id, owner_id, project_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status)
		VALUES ('task-1', $1, 'project-1', 'task-1', 'T', 'b', 'coordination', 'p', 'explicit', 'running')`, owner)
	e.exec(t, `INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider, configuration_revision,
		agent_namespace, agent_name, role, depth, state, capacity_held) VALUES ('attempt-1', 'task-1', 1, 'p', 'claude', 'r', 'ns', 'ag', 'supervisor', 1, 'active', true)`)
	out, _ = e.svc.Smoke(ctx, "project-1", nil)
	if len(out.CapacityHolders) != 1 || out.CapacityHolders[0] != "task-1" || !out.GlobalCapacityAvailable {
		t.Fatalf("capacity: %+v", out)
	}
	e.svc.MaxChildrenGlobal = 1
	if out, _ = e.svc.Smoke(ctx, "project-1", nil); out.GlobalCapacityAvailable {
		t.Fatal("global capacity reported free at the limit")
	}

	// More than 100 deliveries are cut at 100 and flagged.
	e.exec(t, `INSERT INTO deliveries (id, idempotency_key, session_id, state)
		SELECT 'bulk-' || g, 'bulk-' || g, 'session-1', 'uncertain' FROM generate_series(1, 120) g`)
	out, _ = e.svc.Smoke(ctx, "project-1", nil)
	if !out.DeliveriesTruncated || len(out.Deliveries) != 100 {
		t.Fatalf("bound: %d truncated=%v", len(out.Deliveries), out.DeliveriesTruncated)
	}
}

func TestSmokePushes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	push := func(id, repo, branch, state string) {
		e.exec(t, `INSERT INTO git_publications (id, workspace_id, request_id, repository, branch, attempt, state)
			VALUES ($1, 'ws', $1, $2, $3, '{}', $4)`, id, repo, branch, state)
	}
	push("p1", "o/r", "mainloop/x", "pending")
	push("p2", "o/r", "mainloop/x", "confirmed")
	push("p3", "o/r", "other", "confirmed")
	push("p4", "o/elsewhere", "mainloop/x", "confirmed")

	// No branch asked: no push facts.
	out, err := e.svc.Smoke(ctx, "project-1", nil)
	if err != nil || len(out.Pushes) != 0 || out.PushConfirmed {
		t.Fatalf("%+v %v", out, err)
	}
	out, err = e.svc.Smoke(ctx, "project-1", ptr("mainloop/x"))
	if err != nil || len(out.Pushes) != 2 || !out.PushConfirmed || out.PushesTruncated {
		t.Fatalf("%+v %v", out, err)
	}
	for _, p := range out.Pushes {
		if p.Branch != "mainloop/x" || (p.RequestId != "p1" && p.RequestId != "p2") {
			t.Fatalf("a push of another branch or repository: %+v", p)
		}
	}
	e.exec(t, `UPDATE git_publications SET state = 'rejected' WHERE id = 'p2'`)
	if out, _ = e.svc.Smoke(ctx, "project-1", ptr("mainloop/x")); out.PushConfirmed {
		t.Fatal("push confirmed with no confirmed publication")
	}
	for i := range 120 {
		push(fmt.Sprintf("bulk-%d", i), "o/r", "bulk", "pending")
	}
	if out, _ = e.svc.Smoke(ctx, "project-1", ptr("bulk")); !out.PushesTruncated || len(out.Pushes) != 100 {
		t.Fatalf("bound: %d truncated=%v", len(out.Pushes), out.PushesTruncated)
	}
}

// A stop Mainloop gave up confirming is listed even when its task has ended, since an ended task
// gets no owner card for it. A stop still being resolved, a confirmed one and another owner's are not.
func TestViewListsUnresolvedStops(t *testing.T) {
	e := newEnv(t)
	task := func(id, ownerID, status string) {
		project := "project-1"
		if ownerID != owner {
			project = "project-x"
		}
		e.exec(t, `INSERT INTO tasks (id, owner_id, project_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status)
			VALUES ($1, $2, $4, $1, 'T', 'b', 'coordination', 'p', 'explicit', $3)`, id, ownerID, status, project)
	}
	e.exec(t, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('project-x', 'someone-else', 'o', 'x', 'o/x', 'https://x.example/o/x')`)
	attempt := func(id, taskID, stopState string, native bool) {
		deleted := "NULL"
		if native {
			deleted = "now()"
		}
		e.exec(t, `INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider, configuration_revision,
			agent_namespace, agent_name, role, depth, state, stop_state, stop_requested_at, resolve_attempts, native_deleted_at, session_id)
			VALUES ($1, $2, 1, 'p', 'claude', 'r', 'ns', 'ag', 'supervisor', 1, 'draining', $3, now() - interval '1 hour', 8, `+deleted+`, 's-'||$1)`, id, taskID, stopState)
	}
	task("t-cancelled", owner, "cancelled")
	attempt("a-unresolved", "t-cancelled", "unresolved", true)
	task("t-requested", owner, "cancelled")
	attempt("a-requested", "t-requested", "requested", false)
	task("t-other", "someone-else", "cancelled")
	attempt("a-other", "t-other", "unresolved", false)

	v := e.view(t, health.Filter{})
	if len(v.Stops) != 1 {
		t.Fatalf("stops = %+v, want only the unresolved stop of this owner", v.Stops)
	}
	s := v.Stops[0]
	if s.AttemptId != "a-unresolved" || s.TaskId != "t-cancelled" || s.TaskStatus != "cancelled" || s.Looks != 8 || !s.NativeDeleted ||
		time.Since(s.RequestedAt) < 50*time.Minute {
		t.Fatalf("stop = %+v", s)
	}
	if got := e.view(t, health.Filter{TaskID: ptr("t-requested")}); len(got.Stops) != 0 {
		t.Fatalf("a filter on another task listed %+v", got.Stops)
	}
	if got := e.view(t, health.Filter{AttemptID: ptr("a-requested")}); len(got.Stops) != 0 {
		t.Fatalf("a filter on another attempt listed %+v", got.Stops)
	}
}
