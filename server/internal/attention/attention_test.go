package attention_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const owner = "owner-1"

type env struct {
	pool *pgxpool.Pool
	svc  *attention.Service
	api  *attention.AttentionAPI
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testdb.New(t).Pool
	svc := &attention.Service{Pool: pool, OwnerID: owner}
	return &env{pool: pool, svc: svc, api: attention.NewAPI(svc)}
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) session(t *testing.T, id, status string) {
	t.Helper()
	e.exec(t, `INSERT INTO sessions (id, user_id, title, description, prompt, status) VALUES ($1, $2, 'Fix the build', 'd', 'p', $3)`,
		id, owner, status)
}

// hitl writes the attention row the HITL observer would.
func (e *env) hitl(t *testing.T, request, session string) {
	t.Helper()
	e.exec(t, `INSERT INTO attention_items (id, owner_id, kind, source_kind, source_id, session_id, task_id, title, needed, href)
		VALUES ($1, $2, 'approval', 'hitl_request', $3, $4, 'a2a-task', 'Approve tool calls', 'Approve Bash', $5)`,
		"attn_"+request, owner, request, session, "/sessions/"+session)
}

func (e *env) notice(t *testing.T, detail string) int64 {
	t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(),
		`INSERT INTO hitl_diagnostics (owner_id, runtime_session_id, detail) VALUES ($1, 'rs', $2) RETURNING id`,
		owner, detail).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// task writes a task blocked on an owner decision, with its attempt, report and (optionally) session.
func (e *env) blockedTask(t *testing.T, id, session, report string) {
	t.Helper()
	ctx := context.Background()
	e.exec(t, `INSERT INTO tasks (id, owner_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status)
		VALUES ($1, $2, $1, 'Ship it', 'brief', 'coordination', 'p', 'explicit', 'running')`, id, owner)
	att := "att-" + id
	var sess any
	if session != "" {
		sess = session
	}
	e.exec(t, `INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider, configuration_revision,
		agent_namespace, agent_name, role, depth, state, session_id) VALUES ($1, $2, 1, 'p', 'claude', 'r', 'ns', 'ag', 'supervisor', 1, 'active', $3)`,
		att, id, sess)
	e.exec(t, `INSERT INTO task_reports (id, task_id, attempt_id, request_id, request_digest, summary, outcome, owner_action, task_version)
		VALUES ($1, $2, $3, 'req', repeat('a', 64), 'Need a call', 'blocked', 'Pick a region', 1)`, report, id, att)
	if _, err := e.pool.Exec(ctx, `UPDATE tasks SET status = 'blocked', reason = 'owner_decision', current_attempt_id = $2,
		decision_attempt_id = $2, decision_report_id = $3, decision_action = 'Pick a region' WHERE id = $1`, id, att, report); err != nil {
		t.Fatal(err)
	}
}

func (e *env) inboxEvents(t *testing.T) []api.EventInboxUpdated {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT payload FROM events WHERE user_id = $1 AND type = $2 ORDER BY id`,
		owner, events.TypeInboxUpdated)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []api.EventInboxUpdated
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var ev api.EventInboxUpdated
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func (e *env) open(t *testing.T, in attention.NewQueueItem) {
	t.Helper()
	if err := events.InTxPublish(context.Background(), e.pool, func(tx pgx.Tx) ([]events.Draft, error) {
		return attention.Open(context.Background(), tx, owner, in)
	}); err != nil {
		t.Fatal(err)
	}
}

func unread(t *testing.T, ev api.EventInboxUpdated) int {
	t.Helper()
	if ev.UnreadCount == nil {
		t.Fatal("inbox:updated carries no unread_count")
	}
	return *ev.UnreadCount
}

func TestSnapshotListsEverySourceAndMarksSessions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.session(t, "s1", "active")
	e.session(t, "s2", "active")
	e.hitl(t, "req1", "s1")
	e.blockedTask(t, "t1", "s2", "rep1")
	e.open(t, attention.NewQueueItem{ID: "q1", Title: "Which branch?", Content: "Pick one", Options: []string{"main", "dev"}, TaskID: ptr("t-other")})
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}

	snap, err := e.svc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Count != 3 || len(snap.Items) != 3 {
		t.Fatalf("count %d items %d", snap.Count, len(snap.Items))
	}
	byID := map[string]api.AttentionItem{}
	for _, it := range snap.Items {
		byID[it.Id] = it
	}
	h := byID["hitl:req1"]
	if h.QueueItem == nil || h.QueueItem.ItemType != api.QueueItemTypeHitlRequest || h.QueueItem.HitlRequestId == nil || *h.QueueItem.HitlRequestId != "req1" {
		t.Fatalf("hitl item %+v", h)
	}
	if h.TaskId != nil {
		t.Fatal("a HITL item must not expose kagent's task id as a Mainloop task")
	}
	d := byID["task:t1"]
	if d.QueueItem != nil || d.TaskId == nil || *d.TaskId != "t1" || d.Kind != api.AttentionItemKindOwnerDecision || d.Needed != "Pick a region" || d.Href != "/tasks/t1" {
		t.Fatalf("decision item %+v", d)
	}
	q := byID["queue:q1"]
	if q.QueueItem == nil || q.QueueItem.Options == nil || len(*q.QueueItem.Options) != 2 || q.QueueItem.Status != api.QueueItemStatusPending {
		t.Fatalf("queue item %+v", q)
	}
	if len(snap.Sessions) != 2 {
		t.Fatalf("sessions %d", len(snap.Sessions))
	}
	// s1 sits behind a native input request, s2 behind a task decision: both need the owner, but
	// only s1 reads waiting_on_user.
	for _, s := range snap.Sessions {
		wantStatus := api.SessionStatusActive
		if s.Id == "s1" {
			wantStatus = api.SessionStatusWaitingOnUser
		}
		if !s.NeedsOwner || s.Status != wantStatus {
			t.Fatalf("session %+v", s)
		}
	}
	if !e.svc.NeedsOwner(ctx, "s1") || !e.svc.NeedsOwner(ctx, "s2") {
		t.Fatal("NeedsOwner should hold for both sessions")
	}
	if e.svc.NeedsOwner(ctx, "nope") {
		t.Fatal("an unknown session needs no owner")
	}
}

func TestArchivedAndFinishedSessionsNeverCount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.session(t, "live", "active")
	e.session(t, "done", "completed")
	e.session(t, "old", "active")
	e.exec(t, `UPDATE sessions SET archived_at = now() WHERE id = 'old'`)
	e.hitl(t, "r-live", "live")
	e.hitl(t, "r-done", "done")
	e.hitl(t, "r-old", "old")
	snap, err := e.svc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Count != 1 || snap.Items[0].Id != "hitl:r-live" || len(snap.Sessions) != 2 { // live and done (history)
		t.Fatalf("%+v", snap)
	}
	if e.svc.NeedsOwner(ctx, "done") || e.svc.NeedsOwner(ctx, "old") {
		t.Fatal("a finished or archived session needs no owner")
	}
	// The unread count follows the same rule.
	e.open(t, attention.NewQueueItem{ID: "q1", Title: "T", Content: "C"})
	evs := e.inboxEvents(t)
	if len(evs) != 1 || unread(t, evs[0]) != 2 {
		t.Fatalf("events %+v", evs)
	}
}

// The snapshot's sessions are the owner's session list (F4 renders it whole), not only the sessions
// behind an item: each row carries its own needs_owner, and waiting_on_user marks only a native input.
func TestSnapshotSessionsAreTheSessionList(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for id, status := range map[string]string{
		"s-active": "active", "s-pending": "pending", "s-idle": "idle", "s-done": "completed", "s-need": "active",
		"s-queue": "active",
	} {
		e.session(t, id, status)
	}
	e.session(t, "s-archived", "active")
	e.exec(t, `UPDATE sessions SET archived_at = now() WHERE id = 's-archived'`)
	e.hitl(t, "req1", "s-need")
	e.open(t, attention.NewQueueItem{ID: "q1", Title: "T", Content: "C", SessionID: ptr("s-queue")})

	snap, err := e.svc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]api.Session{}
	for _, s := range snap.Sessions {
		got[s.Id] = s
	}
	if len(got) != 6 || len(snap.Sessions) != 6 {
		t.Fatalf("sessions %d: %v", len(snap.Sessions), got)
	}
	for id, want := range map[string]struct {
		status api.SessionStatus
		needs  bool
	}{
		"s-active": {api.SessionStatusActive, false}, "s-pending": {api.SessionStatusPending, false},
		"s-idle": {api.SessionStatusIdle, false}, "s-done": {api.SessionStatusCompleted, false},
		"s-need":  {api.SessionStatusWaitingOnUser, true},
		"s-queue": {api.SessionStatusActive, true}, // a queue question needs the owner, but is no native input
	} {
		if s, ok := got[id]; !ok || s.Status != want.status || s.NeedsOwner != want.needs {
			t.Errorf("%s: %+v want %+v", id, s, want)
		}
	}
	if snap.Count != 2 {
		t.Fatalf("count %d", snap.Count)
	}
}

// An answered HITL request does not prompt again while the answer travels (the rule hides a
// request with a live response); a refused answer brings the request back.
func TestAnsweredHITLRequestLeavesTheInbox(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.session(t, "s1", "active")
	e.hitl(t, "req1", "s1")
	e.exec(t, `INSERT INTO hitl_requests (id, owner_id, session_id, kagent_session_id, agent_namespace, agent_name, context_id, task_id,
		status_message_id, request_hash, kind, payload) VALUES ('req1', $1, 's1', 'ks', 'ns', 'ag', 'ctx', 'a2a-task', 'sm', 'h', 'ask_user', '{}')`, owner)
	for _, state := range []string{"recorded", "sending", "uncertain", "accepted"} {
		e.exec(t, `DELETE FROM hitl_responses`)
		e.exec(t, `INSERT INTO hitl_responses (id, request_id, owner_id, idempotency_key, decision, decision_hash, outbound_message_id, receipt, state)
			VALUES ('act', 'req1', $1, 'k', '{}', 'h', 'm', '{}', $2)`, owner, state)
		snap, _ := e.svc.Snapshot(ctx)
		if snap.Count != 0 || e.svc.NeedsOwner(ctx, "s1") {
			t.Fatalf("%s: an answered request still prompts: %+v", state, snap)
		}
	}
	e.exec(t, `UPDATE hitl_responses SET state = 'rejected_transport'`)
	if snap, _ := e.svc.Snapshot(ctx); snap.Count != 1 || !e.svc.NeedsOwner(ctx, "s1") {
		t.Fatalf("a refused answer should bring the request back: %+v", snap)
	}
}

// Items of a task that has ended, of an attempt that is no longer the task's current one, and of an
// archived or native-deleted attempt are hidden from the list, the count and needs_owner. A queue
// question with a task but no session is tied to no attempt, so only the task ending hides it.
func TestTaskAndAttemptStateHideItems(t *testing.T) {
	for name, hide := range map[string]string{
		"task cancelled":     `UPDATE tasks SET status = 'cancelled', reason = NULL WHERE id = 't1'`,
		"task completed":     `UPDATE tasks SET status = 'completed', reason = NULL WHERE id = 't1'`,
		"attempt replaced":   `UPDATE tasks SET current_attempt_id = NULL WHERE id = 't1'`,
		"attempt superseded": `UPDATE task_attempts SET state = 'superseded' WHERE task_id = 't1'`,
		"attempt archived":   `UPDATE task_attempts SET archived_at = now() WHERE task_id = 't1'`,
		"native deleted":     `UPDATE task_attempts SET native_deleted_at = now() WHERE task_id = 't1'`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			e.session(t, "s1", "active")
			e.blockedTask(t, "t1", "s1", "rep1")
			e.hitl(t, "req1", "s1")
			e.open(t, attention.NewQueueItem{ID: "q1", Title: "T", Content: "C", SessionID: ptr("s1"), TaskID: ptr("t1")})
			e.open(t, attention.NewQueueItem{ID: "q2", Title: "T", Content: "C", TaskID: ptr("t1")}) // no session
			if err := e.svc.SyncTasks(ctx); err != nil {
				t.Fatal(err)
			}
			if snap, _ := e.svc.Snapshot(ctx); snap.Count != 4 || !e.svc.NeedsOwner(ctx, "s1") {
				t.Fatalf("before: %d items", snap.Count)
			}
			e.exec(t, hide)
			snap, err := e.svc.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			left := 0
			if !strings.HasPrefix(name, "task ") {
				left = 1 // queue:q2
			}
			if snap.Count != left || e.svc.NeedsOwner(ctx, "s1") {
				t.Fatalf("%d items still visible: %+v", snap.Count, snap.Items)
			}
		})
	}
}

// A session that finishes or is archived hides its items without any write in this package; the
// sweep closes them and publishes the new unread count.
func TestSweepClosesItemsOfFinishedSessionsAndPublishes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.session(t, "s1", "active")
	e.session(t, "s2", "active")
	e.hitl(t, "req1", "s1")
	e.hitl(t, "req2", "s2")
	if err := e.svc.SyncTasks(ctx); err != nil { // the first pass only learns the count
		t.Fatal(err)
	}
	if n := len(e.inboxEvents(t)); n != 0 {
		t.Fatalf("%d events from a quiet first pass", n)
	}
	e.exec(t, `UPDATE sessions SET status = 'cancelled' WHERE id = 's1'`)
	e.exec(t, `UPDATE sessions SET archived_at = now() WHERE id = 's2'`)
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	evs := e.inboxEvents(t)
	if len(evs) != 1 || unread(t, evs[0]) != 0 {
		t.Fatalf("events %+v", evs)
	}
	var open int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM attention_items WHERE state = 'open'`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("open %d err %v", open, err)
	}
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(e.inboxEvents(t)); n != 1 {
		t.Fatalf("a quiet pass published: %d events", n)
	}
	// A task that ends moves the count with no item written: the sweep still publishes.
	e.session(t, "s3", "active")
	e.blockedTask(t, "t1", "s3", "rep1")
	e.hitl(t, "req3", "s3")
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	e.exec(t, `UPDATE tasks SET status = 'cancelled', reason = NULL WHERE id = 't1'`)
	before := len(e.inboxEvents(t))
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	evs = e.inboxEvents(t)
	if len(evs) != before+1 || unread(t, evs[len(evs)-1]) != 0 {
		t.Fatalf("events %+v", evs)
	}
}

func TestOpenRejectsNoticeIDs(t *testing.T) {
	e := newEnv(t)
	err := events.InTxPublish(context.Background(), e.pool, func(tx pgx.Tx) ([]events.Draft, error) {
		return attention.Open(context.Background(), tx, owner, attention.NewQueueItem{ID: "diag_1", Title: "T", Content: "C"})
	})
	if !errors.Is(err, attention.ErrInvalid) {
		t.Fatalf("%v", err)
	}
}

// A question the sweep closed with no answer reads Closed, not Answered.
func TestUnansweredClosedQuestionReadsClosed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.blockedTask(t, "t1", "", "rep1")
	e.open(t, attention.NewQueueItem{ID: "q1", Title: "Pick", Content: "C", Needed: "Pick one", TaskID: ptr("t1")})
	e.exec(t, `UPDATE tasks SET status = 'cancelled', reason = NULL WHERE id = 't1'`)
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := e.svc.Activity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Id == "queue:q1" {
			if it.Content != "Closed: Pick one" {
				t.Fatalf("content %q", it.Content)
			}
			return
		}
	}
	t.Fatalf("no activity for the closed question: %+v", items)
}

func TestTaskDecisionSweepOpensAndResolves(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.blockedTask(t, "t1", "", "rep1")
	for i := 0; i < 2; i++ { // the second pass changes nothing
		if err := e.svc.SyncTasks(ctx); err != nil {
			t.Fatal(err)
		}
	}
	snap, _ := e.svc.Snapshot(ctx)
	if snap.Count != 1 || snap.Items[0].Id != "task:t1" || len(snap.Items[0].SessionIds) != 0 {
		t.Fatalf("%+v", snap)
	}
	if evs := e.inboxEvents(t); len(evs) != 1 || unread(t, evs[0]) != 1 {
		t.Fatalf("one event for one change, got %+v", evs)
	}

	// The decision clears: the item resolves and is never reopened for the same report.
	e.exec(t, `UPDATE tasks SET status = 'running', reason = NULL, decision_attempt_id = NULL, decision_report_id = NULL, decision_action = NULL WHERE id = 't1'`)
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if snap, _ = e.svc.Snapshot(ctx); snap.Count != 0 {
		t.Fatalf("%+v", snap)
	}
	evs := e.inboxEvents(t)
	if len(evs) != 2 || unread(t, evs[1]) != 0 {
		t.Fatalf("events %+v", evs)
	}
	e.exec(t, `UPDATE tasks SET status = 'blocked', reason = 'owner_decision', decision_attempt_id = 'att-t1', decision_report_id = 'rep1', decision_action = 'Pick a region' WHERE id = 't1'`)
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if snap, _ = e.svc.Snapshot(ctx); snap.Count != 0 {
		t.Fatal("an answered decision must not reopen")
	}
}

func TestTaskDecisionResolvesWhenAttemptEndsOrTaskFinishes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.blockedTask(t, "t1", "", "rep1")
	e.blockedTask(t, "t2", "", "rep2")
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if snap, _ := e.svc.Snapshot(ctx); snap.Count != 2 {
		t.Fatalf("%d", snap.Count)
	}
	e.exec(t, `UPDATE task_attempts SET state = 'fenced', capacity_held = false WHERE id = 'att-t1'`)
	e.exec(t, `UPDATE tasks SET status = 'cancelled', reason = NULL WHERE id = 't2'`)
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if snap, _ := e.svc.Snapshot(ctx); snap.Count != 0 {
		t.Fatalf("%+v", snap.Items)
	}
}

func TestActivity(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.blockedTask(t, "t1", "", "rep1")
	n1 := e.notice(t, "The request no longer exists.")
	e.open(t, attention.NewQueueItem{ID: "q1", Title: "Which branch?", Content: "Pick", TaskID: ptr("t1")})
	if err := e.api.Svc.Respond(ctx, "q1", api.QueueItemResponse{Response: "main"}); err != nil {
		t.Fatal(err)
	}
	items, err := e.svc.Activity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("%+v", items)
	}
	byID := map[string]api.ActivityItem{}
	for _, it := range items {
		byID[it.Id] = it
	}
	rep := byID["report:rep1"]
	if rep.Dismissible || rep.QueueItemId != nil || rep.Content != "Provider report (blocked; unverified): Need a call" || rep.Href == nil || *rep.Href != "/tasks/t1" {
		t.Fatalf("report %+v", rep)
	}
	noticeKey := fmt.Sprintf("queue:diag_%d", n1)
	nt := byID[noticeKey]
	if !nt.Dismissible || nt.QueueItemId == nil || *nt.QueueItemId != fmt.Sprintf("diag_%d", n1) {
		t.Fatalf("notice %+v", nt)
	}
	ans := byID["queue:q1"]
	if ans.Dismissible || ans.QueueItemId == nil || *ans.QueueItemId != "q1" {
		t.Fatalf("answered %+v", ans)
	}
	for i := 1; i < len(items); i++ {
		if items[i].CreatedAt.After(items[i-1].CreatedAt) {
			t.Fatal("not newest first")
		}
	}

	// Dismissed notices leave Activity.
	if err := e.svc.Dismiss(ctx, fmt.Sprintf("diag_%d", n1)); err != nil {
		t.Fatal(err)
	}
	if items, _ = e.svc.Activity(ctx); len(items) != 2 {
		t.Fatalf("%+v", items)
	}
}

func TestActivityIsCapped(t *testing.T) {
	e := newEnv(t)
	e.exec(t, `INSERT INTO hitl_diagnostics (owner_id, runtime_session_id, detail) SELECT $1, 'rs', 'n' || g FROM generate_series(1, 130) g`, owner)
	items, err := e.svc.Activity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 100 {
		t.Fatalf("%d", len(items))
	}
}

func TestRespondIsIdempotentAndFenced(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.session(t, "s1", "active")
	e.hitl(t, "req1", "s1")
	e.open(t, attention.NewQueueItem{ID: "q1", Title: "Which branch?", Content: "Pick", SessionID: ptr("s1")})
	meta := map[string]interface{}{"k": "v"}
	body := api.QueueItemResponse{Response: "main", Metadata: &meta}

	if err := e.svc.Respond(ctx, "q1", body); err != nil {
		t.Fatal(err)
	}
	n := len(e.inboxEvents(t))
	if err := e.svc.Respond(ctx, "q1", body); err != nil {
		t.Fatalf("a repeat of the same answer: %v", err)
	}
	if len(e.inboxEvents(t)) != n {
		t.Fatal("a repeat published a second event")
	}
	if err := e.svc.Respond(ctx, "q1", api.QueueItemResponse{Response: "dev"}); !errors.Is(err, attention.ErrConflict) {
		t.Fatalf("a different answer: %v", err)
	}
	if err := e.svc.Respond(ctx, "req1", body); !errors.Is(err, attention.ErrConflict) {
		t.Fatalf("a HITL id: %v", err)
	}
	id := e.notice(t, "x")
	if err := e.svc.Respond(ctx, fmt.Sprintf("diag_%d", id), body); !errors.Is(err, attention.ErrConflict) {
		t.Fatalf("a notice: %v", err)
	}
	if err := e.svc.Respond(ctx, "nope", body); !errors.Is(err, attention.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if err := e.svc.Respond(ctx, "q1", api.QueueItemResponse{Response: "  "}); !errors.Is(err, attention.ErrInvalid) {
		t.Fatalf("empty: %v", err)
	}
	snap, _ := e.svc.Snapshot(ctx)
	if snap.Count != 1 || snap.Items[0].Id != "hitl:req1" {
		t.Fatalf("an answered item must leave Needs you: %+v", snap.Items)
	}
	evs := e.inboxEvents(t)
	last := evs[len(evs)-1]
	if unread(t, last) != 1 || last.ItemId == nil || *last.ItemId != "attn_queue_q1" {
		t.Fatalf("%+v", last)
	}
}

func TestDismissIsIdempotent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.open(t, attention.NewQueueItem{ID: "q1", Title: "T", Content: "C"})
	id := fmt.Sprintf("diag_%d", e.notice(t, "x"))
	if err := e.svc.Dismiss(ctx, id); err != nil {
		t.Fatal(err)
	}
	n := len(e.inboxEvents(t))
	if err := e.svc.Dismiss(ctx, id); err != nil {
		t.Fatalf("again: %v", err)
	}
	if len(e.inboxEvents(t)) != n {
		t.Fatal("a repeat published an event")
	}
	if err := e.svc.Dismiss(ctx, "q1"); !errors.Is(err, attention.ErrConflict) {
		t.Fatalf("queue: %v", err)
	}
	if err := e.svc.Dismiss(ctx, "diag_999999"); !errors.Is(err, attention.ErrNotFound) {
		t.Fatalf("unknown notice: %v", err)
	}
	if err := e.svc.Dismiss(ctx, "nope"); !errors.Is(err, attention.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

// The event is written in the change's own transaction: a change that rolls back leaves no event,
// and the event cannot outlive a change.
func TestInboxEventRollsBackWithTheChange(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	boom := errors.New("boom")
	err := events.InTxPublish(ctx, e.pool, func(tx pgx.Tx) ([]events.Draft, error) {
		drafts, err := attention.Open(ctx, tx, owner, attention.NewQueueItem{ID: "q1", Title: "T", Content: "C"})
		if err != nil || len(drafts) != 1 {
			t.Fatalf("drafts %d err %v", len(drafts), err)
		}
		return drafts, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	if n := len(e.inboxEvents(t)); n != 0 {
		t.Fatalf("%d events survived a rollback", n)
	}
	if snap, _ := e.svc.Snapshot(ctx); snap.Count != 0 {
		t.Fatal("the item survived a rollback")
	}
	// Opening the same id twice publishes once.
	e.open(t, attention.NewQueueItem{ID: "q1", Title: "T", Content: "C"})
	e.open(t, attention.NewQueueItem{ID: "q1", Title: "T", Content: "C"})
	if n := len(e.inboxEvents(t)); n != 1 {
		t.Fatalf("%d events", n)
	}
}

func TestUnreadCountIgnoresRead(t *testing.T) {
	e := newEnv(t)
	e.open(t, attention.NewQueueItem{ID: "q1", Title: "T", Content: "C"})
	e.open(t, attention.NewQueueItem{ID: "q2", Title: "T", Content: "C"})
	e.exec(t, `UPDATE attention_items SET read_at = now() WHERE source_id = 'q1'`)
	e.notice(t, "x") // a notice never counts
	if err := e.svc.Respond(context.Background(), "q2", api.QueueItemResponse{Response: "a"}); err != nil {
		t.Fatal(err)
	}
	evs := e.inboxEvents(t)
	if got := unread(t, evs[len(evs)-1]); got != 0 {
		t.Fatalf("unread %d", got)
	}
	// q1 is read but still unresolved: it counts for the badge, not for unread.
	if snap, _ := e.svc.Snapshot(context.Background()); snap.Count != 1 {
		t.Fatalf("count %d", snap.Count)
	}
}

// The F5 MSW fixtures (web/src/features/inbox/test/fixtures.ts) type these keys as required, with
// null for an absent value. The Go answers must carry every one, and decode strictly into the
// generated types, so the two cannot drift.
func TestAnswersMatchTheFrontendFixtures(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.session(t, "s1", "active")
	e.hitl(t, "req1", "s1")
	e.blockedTask(t, "t1", "", "rep1")
	e.open(t, attention.NewQueueItem{ID: "q1", Title: "Which branch?", Content: "Pick", Options: []string{"a"}, TaskID: ptr("t1")})
	e.notice(t, "gone")
	if err := e.svc.SyncTasks(ctx); err != nil {
		t.Fatal(err)
	}
	resp, err := e.api.GetAttention(ctx, api.GetAttentionRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	raw := roundTrip(t, resp, &api.AttentionSnapshot{})
	requireKeys(t, "snapshot", raw, "items", "count", "sessions")
	for _, it := range raw["items"].([]any) {
		m := it.(map[string]any)
		requireKeys(t, "attention item", m, "id", "kind", "title", "needed", "href", "session_ids", "task_id", "queue_item")
		if q, ok := m["queue_item"].(map[string]any); ok {
			requireKeys(t, "queue item", q, "id", "main_thread_id", "task_id", "user_id", "hitl_request_id", "item_type", "priority",
				"title", "content", "context", "options", "status", "response", "responded_at", "read_at", "created_at", "expires_at")
		}
	}
	act, err := e.api.ListActivity(ctx, api.ListActivityRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	var rows []any
	b, _ := json.Marshal(act)
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var typed []api.ActivityItem
	if err := dec.Decode(&typed); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(b, &rows)
	for _, r := range rows {
		m := r.(map[string]any)
		requireKeys(t, "activity item", m, "id", "title", "content", "href", "created_at", "queue_item_id", "dismissible")
		isNotice := m["queue_item_id"] != nil && m["dismissible"] == true
		isReport := m["queue_item_id"] == nil && m["dismissible"] == false
		answered := m["queue_item_id"] != nil && m["dismissible"] == false
		if !isNotice && !isReport && !answered {
			t.Fatalf("a row F5 would drop: %v", m)
		}
	}
	rr, err := e.api.RespondToQueueItem(ctx, api.RespondToQueueItemRequestObject{ItemId: "q1", Body: &api.QueueItemResponse{Response: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	ok := roundTrip(t, rr, &api.QueueRespondResponse{})
	if ok["status"] != "ok" || ok["message"] != "Response sent" {
		t.Fatalf("%v", ok)
	}
	dr, err := e.api.DismissQueueDiagnostic(ctx, api.DismissQueueDiagnosticRequestObject{ItemId: "q1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, isConflict := dr.(api.DismissQueueDiagnostic409JSONResponse); !isConflict {
		t.Fatalf("%T", dr)
	}
	nr, err := e.api.RespondToQueueItem(ctx, api.RespondToQueueItemRequestObject{ItemId: "nope", Body: &api.QueueItemResponse{Response: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, isNotFound := nr.(api.RespondToQueueItem404JSONResponse); !isNotFound {
		t.Fatalf("%T", nr)
	}
	er, _ := e.api.RespondToQueueItem(ctx, api.RespondToQueueItemRequestObject{ItemId: "q1", Body: &api.QueueItemResponse{}})
	if _, isInvalid := er.(api.RespondToQueueItem422JSONResponse); !isInvalid {
		t.Fatalf("%T", er)
	}
}

// roundTrip marshals an answer, decodes it strictly into the generated type, and returns the
// generic form for key checks.
func roundTrip(t *testing.T, v any, into any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func requireKeys(t *testing.T, what string, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("%s lacks %q: %v", what, k, m)
		}
	}
}

func ptr[T any](v T) *T { return &v }
