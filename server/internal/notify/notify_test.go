package notify_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/notify"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

const owner = "owner-1"

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// fakeNtfy records publishes and serves them back on the poll endpoint.
type fakeNtfy struct {
	mu        sync.Mutex
	published []map[string]any
	// failNext makes the next publish store the message but answer 502, as if the answer was lost.
	failNext bool
	// reject answers every publish with this status without storing it.
	reject int
	auth   []string
}

func (f *fakeNtfy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/":
		if f.reject != 0 {
			w.WriteHeader(f.reject)
			return
		}
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m["event"] = "message"
		f.published = append(f.published, m)
		if f.failNext {
			f.failNext = false
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(m)
	case r.Method == http.MethodGet && r.URL.Path == "/alerts/json" && r.URL.Query().Get("poll") == "1":
		for _, m := range f.published {
			_ = json.NewEncoder(w).Encode(m)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeNtfy) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.published)
}

type env struct {
	db   *testdb.DB
	svc  *notify.Service
	ntfy *fakeNtfy
	runs int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	f := &fakeNtfy{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &env{db: d, ntfy: f, svc: &notify.Service{
		Pool: d.Pool, OwnerID: owner, URL: srv.URL, Topic: "alerts", Token: "tk", AppURL: "https://ml.test",
	}}
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.db.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// drain claims and handles every due row once, ignoring backoff.
func (e *env) drain(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	e.exec(t, `UPDATE push_notifications SET next_attempt_at = now() - interval '1 second'`)
	e.runs++
	claims, err := notify.Table().Claim(ctx, e.db.Pool, "test", time.Minute, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claims {
		out, herr := e.svc.Handle(ctx, c)
		if out != reconcile.Recorded() {
			// A retry: release the lease the way the runner would.
			if err := c.RetryAfter(ctx, e.db.Pool, 0); err != nil {
				t.Fatal(err)
			}
			_ = herr
		}
	}
}

func (e *env) state(t *testing.T, key string) string {
	t.Helper()
	var s string
	if err := e.db.Pool.QueryRow(context.Background(),
		`SELECT state FROM push_notifications WHERE idempotency_key = $1`, key).Scan(&s); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return s
}

func (e *env) enqueue(t *testing.T) {
	t.Helper()
	if err := e.svc.Enqueue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (e *env) openItem(t *testing.T, id, kind string) {
	t.Helper()
	e.exec(t, `INSERT INTO attention_items (id, owner_id, kind, source_kind, source_id, title, needed, href)
		VALUES ($1, $2, $3, 'queue', $1, 'Approve the merge', 'Merge PR #7 into main?', '/inbox')`, id, owner, kind)
}

func (e *env) task(t *testing.T, id, parent, status string) {
	t.Helper()
	root := id
	var p any
	if parent != "" {
		root, p = parent, parent
	}
	e.exec(t, `INSERT INTO tasks (id, owner_id, parent_task_id, root_task_id, title, brief, mode, assigned_profile_id,
		selection_source, status) VALUES ($1, $2, $3, $4, 'Ship push', 'brief', 'coordination', 'p', 'explicit', $5)`,
		id, owner, p, root, status)
}

func TestAttentionItemNotifiesOnceWithLinkAndPriority(t *testing.T) {
	e := newEnv(t)
	e.enqueue(t) // sets the floor
	e.openItem(t, "a1", "approval")
	e.openItem(t, "a2", "question")
	for range 3 {
		e.enqueue(t)
		e.drain(t)
	}
	if got := e.ntfy.count(); got != 2 {
		t.Fatalf("published %d, want 2 (one per item, however often the sweep runs)", got)
	}
	byTitle := map[float64]map[string]any{}
	for _, m := range e.ntfy.published {
		byTitle[m["priority"].(float64)] = m
		if m["click"] != "https://ml.test/inbox" || m["topic"] != "alerts" || m["message"] != "Merge PR #7 into main?" {
			t.Fatalf("message: %+v", m)
		}
	}
	if byTitle[4] == nil || byTitle[3] == nil {
		t.Fatalf("approval must be priority 4 and a question 3: %+v", e.ntfy.published)
	}
	if e.ntfy.auth[0] != "Bearer tk" {
		t.Fatalf("auth header: %q", e.ntfy.auth[0])
	}
}

func TestItemsOpenBeforeEnablingAreNotSent(t *testing.T) {
	e := newEnv(t)
	e.openItem(t, "old", "approval")
	e.exec(t, `UPDATE attention_items SET created_at = now() - interval '1 hour'`)
	e.enqueue(t)
	e.drain(t)
	if got := e.ntfy.count(); got != 0 {
		t.Fatalf("published %d for an item older than the floor", got)
	}
}

func TestProblemsAreNotPushed(t *testing.T) {
	e := newEnv(t)
	e.enqueue(t)
	e.exec(t, `INSERT INTO attention_items (id, owner_id, kind, source_kind, source_id, title, needed, href)
		VALUES ('p1', $1, 'problem', 'queue', 'p1', 'Stalled', 'Recovering', '/')`, owner)
	e.enqueue(t)
	e.drain(t)
	if got := e.ntfy.count(); got != 0 {
		t.Fatalf("published %d for a problem", got)
	}
}

func TestHandledItemIsSuppressed(t *testing.T) {
	e := newEnv(t)
	e.enqueue(t)
	e.openItem(t, "a1", "approval")
	e.enqueue(t)
	e.exec(t, `UPDATE attention_items SET state = 'resolved' WHERE id = 'a1'`)
	e.drain(t)
	if got := e.ntfy.count(); got != 0 {
		t.Fatalf("published %d for a handled item", got)
	}
	if s := e.state(t, "attention:a1"); s != "suppressed" {
		t.Fatalf("state %s", s)
	}
}

func TestUncertainSendIsNotRepeated(t *testing.T) {
	e := newEnv(t)
	e.enqueue(t)
	e.openItem(t, "a1", "approval")
	e.enqueue(t)
	e.ntfy.failNext = true // ntfy keeps it but the answer is a 502
	e.drain(t)
	if s := e.state(t, "attention:a1"); s != "pending" {
		t.Fatalf("after the lost answer: %s", s)
	}
	e.drain(t)
	if got := e.ntfy.count(); got != 1 {
		t.Fatalf("published %d, want 1: the retry must find the first copy", got)
	}
	if s := e.state(t, "attention:a1"); s != "sent" {
		t.Fatalf("state %s", s)
	}
}

func TestRejectedPublishFails(t *testing.T) {
	e := newEnv(t)
	e.enqueue(t)
	e.openItem(t, "a1", "approval")
	e.enqueue(t)
	e.ntfy.reject = http.StatusForbidden
	e.drain(t)
	if s := e.state(t, "attention:a1"); s != "failed" {
		t.Fatalf("state %s", s)
	}
}

func TestRootTaskFinishNotifies(t *testing.T) {
	e := newEnv(t)
	e.enqueue(t)
	e.task(t, "t1", "", "running")
	e.task(t, "c1", "t1", "completed") // a child: not pushed
	e.enqueue(t)
	e.drain(t)
	if got := e.ntfy.count(); got != 0 {
		t.Fatalf("published %d before the root finished", got)
	}
	e.exec(t, `UPDATE tasks SET status = 'failed', updated_at = now() WHERE id = 't1'`)
	for range 3 {
		e.enqueue(t)
		e.drain(t)
	}
	if got := e.ntfy.count(); got != 1 {
		t.Fatalf("published %d, want 1", got)
	}
	m := e.ntfy.published[0]
	if m["title"] != "Ship push" || m["message"] != "Task failed" || m["click"] != "https://ml.test/tasks/t1" || m["priority"].(float64) != 4 {
		t.Fatalf("message: %+v", m)
	}
	if s := e.state(t, "task:t1:failed:0"); s != "sent" {
		t.Fatalf("state %s", s)
	}
}

func TestOwnerDecisionBlockIsLeftToAttention(t *testing.T) {
	e := newEnv(t)
	e.enqueue(t)
	e.exec(t, `INSERT INTO tasks (id, owner_id, root_task_id, title, brief, mode, assigned_profile_id,
		selection_source, status, reason) VALUES ('t1', $1, 't1', 'x', 'b', 'coordination', 'p', 'explicit', 'blocked', 'owner_decision')`, owner)
	e.enqueue(t)
	e.drain(t)
	if got := e.ntfy.count(); got != 0 {
		t.Fatalf("published %d for an owner-decision block", got)
	}
}
