package apns_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/apns"
	"github.com/oldsj/mainloop/server/internal/attention"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/notify"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

const owner = "owner-1"

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// fakeSender records what would go to APNs and answers with the next queued error.
type fakeSender struct {
	mu   sync.Mutex
	sent []apns.Request
	errs []error
	res  apns.Result
}

func (f *fakeSender) Send(_ context.Context, r apns.Request) (apns.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, r)
	if len(f.errs) == 0 {
		return apns.Result{Status: 200}, nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return f.res, err
}

func (f *fakeSender) requests() []apns.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apns.Request(nil), f.sent...)
}

type env struct {
	db     *testdb.DB
	notify *notify.Service
	svc    *apns.Service
	sender *fakeSender
}

// newEnv has ntfy off, so notifications exist for APNs alone.
func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	s := &fakeSender{}
	e := &env{
		db: d, sender: s,
		notify: &notify.Service{Pool: d.Pool, OwnerID: owner},
		svc: &apns.Service{
			Pool: d.Pool, OwnerID: owner, Sender: s,
			Attention: &attention.Service{Pool: d.Pool, OwnerID: owner},
		},
	}
	e.sweep(t) // turns push on: items opened before this are not sent
	return e
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.db.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (e *env) scalar(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := e.db.Pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// sweep runs both enqueue sweeps.
func (e *env) sweep(t *testing.T) {
	t.Helper()
	if err := e.notify.Enqueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Enqueue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// drain claims and handles every due delivery once, ignoring backoff.
func (e *env) drain(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	e.exec(t, `UPDATE apns_deliveries SET next_attempt_at = now() - interval '1 second'`)
	claims, err := apns.Table().Claim(ctx, e.db.Pool, "test", time.Minute, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claims {
		if out, _ := e.svc.Handle(ctx, c); out != reconcile.Recorded() {
			if err := c.RetryAfter(ctx, e.db.Pool, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// device registers a phone; with a token, it has set that APNs token.
func (e *env) device(t *testing.T, n int, token string) string {
	t.Helper()
	id := "dev_" + strings.Repeat(string(rune('a'+n)), 26)
	coord := strings.Repeat(string(rune('A'+n)), 43)
	e.exec(t, `INSERT INTO owner_devices (id, owner_id, name, platform, request_key_x, request_key_y, key_thumbprint,
		registered_by, registration_auth_time) VALUES ($1, $2, 'Phone', 'ios', $3, $3, $3, 'owner-sub', now())`, id, owner, coord)
	if token != "" {
		e.exec(t, `INSERT INTO device_push_tokens (device_id, token, environment) VALUES ($1, $2, 'production')`, id, token)
	}
	return id
}

// approval opens an approval whose content holds what must never reach a lock screen.
func (e *env) approval(t *testing.T, id string) {
	t.Helper()
	detail, _ := json.Marshal(map[string]any{
		"item_type": "approval", "priority": "high",
		"content": "kubectl create secret generic db --from-literal=password=hunter2-fixture\n--- a/x\n+++ b/x",
	})
	e.exec(t, `INSERT INTO attention_items (id, owner_id, kind, source_kind, source_id, title, needed, href, detail)
		VALUES ($1, $2, 'approval', 'queue', $3, 'Run kubectl create on internal', 'Approve: Run kubectl create on internal?', '/inbox', $4)`,
		"attn_"+id, owner, id, detail)
}

func (e *env) deliveries(t *testing.T) map[string]string {
	t.Helper()
	rows, err := e.db.Pool.Query(context.Background(), `SELECT device_id, state FROM apns_deliveries`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var d, s string
		if err := rows.Scan(&d, &s); err != nil {
			t.Fatal(err)
		}
		out[d] = s
	}
	return out
}

func TestAnApprovalReachesEachPhoneOnce(t *testing.T) {
	e := newEnv(t)
	tokA, tokB := strings.Repeat("a1", 32), strings.Repeat("b2", 32)
	a, b := e.device(t, 0, tokA), e.device(t, 1, tokB)
	e.device(t, 2, "") // no token: nothing to send to
	e.approval(t, "q1")
	e.sweep(t)
	e.sweep(t)
	e.drain(t)
	e.drain(t)

	sent := e.sender.requests()
	if len(sent) != 2 {
		t.Fatalf("sent %d, want one per phone with a token", len(sent))
	}
	if d := e.deliveries(t); len(d) != 2 || d[a] != "sent" || d[b] != "sent" {
		t.Fatalf("deliveries = %v", d)
	}
	pushID := e.scalar(t, `SELECT id FROM push_notifications WHERE idempotency_key = 'attention:attn_q1'`)
	if s := e.scalar(t, `SELECT state FROM push_notifications WHERE id = $1`, pushID); s != "skipped" {
		t.Fatalf("with ntfy off its row is %q, want skipped", s)
	}
	tokens := map[string]bool{}
	for _, r := range sent {
		tokens[r.DeviceToken] = true
		if r.Environment != "production" || r.CollapseID != pushID || r.Priority != 10 || len(r.ID) != 36 ||
			!r.Expiration.After(time.Now().Add(11*time.Hour)) {
			t.Fatalf("request = %+v", r)
		}
		if strings.Contains(string(r.Payload), "hunter2") || strings.Contains(string(r.Payload), "kubectl create secret") ||
			strings.Contains(string(r.Payload), "+++") {
			t.Fatalf("the payload carries the item's content: %s", r.Payload)
		}
	}
	if !tokens[tokA] || !tokens[tokB] {
		t.Fatalf("sent to %v", tokens)
	}

	var p struct {
		APS struct {
			Alert             struct{ Title, Body string }
			Sound             string
			Badge             *int
			ThreadID          string `json:"thread-id"`
			Category          string
			InterruptionLevel string `json:"interruption-level"`
		}
		Mainloop map[string]string
	}
	if err := json.Unmarshal(sent[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.APS.Alert.Title != "Run kubectl create on internal" || p.APS.Alert.Body != "Approve: Run kubectl create on internal?" ||
		p.APS.Sound != "default" || p.APS.Badge == nil || *p.APS.Badge != 1 || p.APS.ThreadID != "needs-you" ||
		p.APS.Category != apns.CategoryNeedsYou || p.APS.InterruptionLevel != "time-sensitive" {
		t.Fatalf("aps = %+v", p.APS)
	}
	if want := map[string]string{"type": "attention", "item_id": "queue:q1", "href": "/inbox"}; !equal(p.Mainloop, want) {
		t.Fatalf("mainloop = %v, want %v", p.Mainloop, want)
	}
}

func TestATokenSetLaterDoesNotPageForOldItems(t *testing.T) {
	e := newEnv(t)
	id := e.device(t, 0, "")
	e.approval(t, "q1")
	e.sweep(t)
	e.exec(t, `INSERT INTO device_push_tokens (device_id, token, environment) VALUES ($1, $2, 'development')`, id, strings.Repeat("c3", 32))
	e.sweep(t)
	if d := e.deliveries(t); len(d) != 0 {
		t.Fatalf("deliveries = %v", d)
	}
	e.approval(t, "q2")
	e.sweep(t)
	e.drain(t)
	if sent := e.sender.requests(); len(sent) != 1 || sent[0].Environment != "development" {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestHandledOrRevokedIsNotSent(t *testing.T) {
	e := newEnv(t)
	a := e.device(t, 0, strings.Repeat("a1", 32))
	b := e.device(t, 1, strings.Repeat("b2", 32))
	e.approval(t, "q1")
	e.sweep(t)
	e.exec(t, `UPDATE owner_devices SET revoked_at = now() WHERE id = $1`, b)
	e.exec(t, `UPDATE attention_items SET state = 'resolved' WHERE id = 'attn_q1'`)
	e.drain(t)
	if sent := e.sender.requests(); len(sent) != 0 {
		t.Fatalf("sent %d", len(sent))
	}
	if d := e.deliveries(t); d[a] != "suppressed" || d[b] != "suppressed" {
		t.Fatalf("deliveries = %v", d)
	}
}

func TestARefusedTokenStopsPush(t *testing.T) {
	e := newEnv(t)
	tok := strings.Repeat("a1", 32)
	id := e.device(t, 0, tok)
	e.approval(t, "q1")
	e.sweep(t)
	e.sender.errs = []error{errors.Join(apns.ErrBadToken, errors.New("status 410 Unregistered"))}
	e.sender.res = apns.Result{Status: 410, Reason: "Unregistered"}
	e.drain(t)
	if d := e.deliveries(t); d[id] != "failed" {
		t.Fatalf("deliveries = %v", d)
	}
	if r := e.scalar(t, `SELECT coalesce(invalid_reason, '') FROM device_push_tokens WHERE device_id = $1`, id); r != "Unregistered" {
		t.Fatalf("invalid_reason = %q", r)
	}

	// Nothing more goes to the refused token. Setting a token again counts as a new one: it gets
	// what opens from then on, not what opened while push was stopped.
	e.approval(t, "q2")
	e.sweep(t)
	if d := e.deliveries(t); len(d) != 1 {
		t.Fatalf("deliveries after refusal = %v", d)
	}
	if _, err := dbq.New(e.db.Pool).SetDevicePushToken(context.Background(), dbq.SetDevicePushTokenParams{
		DeviceID: id, Token: strings.Repeat("d4", 32), Environment: "production",
	}); err != nil {
		t.Fatal(err)
	}
	e.sweep(t)
	e.approval(t, "q3")
	e.sweep(t)
	e.drain(t)
	sent := e.sender.requests()
	if len(sent) != 2 || sent[1].DeviceToken != strings.Repeat("d4", 32) || !strings.Contains(string(sent[1].Payload), "queue:q3") {
		t.Fatalf("sent = %d, want q1 then only q3", len(sent))
	}
}

func TestUncertainSendsRetryThenGiveUp(t *testing.T) {
	e := newEnv(t)
	id := e.device(t, 0, strings.Repeat("a1", 32))
	e.approval(t, "q1")
	e.sweep(t)
	for range apns.MaxAttempts {
		e.sender.errs = append(e.sender.errs, errors.New("apns: status 503 ServiceUnavailable"))
	}
	for i := 1; i < apns.MaxAttempts; i++ {
		e.drain(t)
		if d := e.deliveries(t); d[id] != "pending" {
			t.Fatalf("after attempt %d: %v", i, d)
		}
	}
	e.drain(t)
	if d := e.deliveries(t); d[id] != "failed" {
		t.Fatalf("after %d attempts: %v", apns.MaxAttempts, e.deliveries(t))
	}
	// Every attempt named the same delivery, so the phone shows at most one copy.
	sent := e.sender.requests()
	for _, r := range sent {
		if r.ID != sent[0].ID || r.CollapseID != sent[0].CollapseID {
			t.Fatalf("attempts differ: %+v vs %+v", r, sent[0])
		}
	}
}

func TestATaskReportIsItsOwnThread(t *testing.T) {
	e := newEnv(t)
	e.device(t, 0, strings.Repeat("a1", 32))
	e.exec(t, `INSERT INTO tasks (id, owner_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, status)
		VALUES ('t1', $1, 't1', 'Ship push', 'brief', 'coordination', 'p', 'explicit', 'completed')`, owner)
	e.sweep(t)
	e.drain(t)
	sent := e.sender.requests()
	if len(sent) != 1 {
		t.Fatalf("sent %d", len(sent))
	}
	var p struct {
		APS struct {
			Alert             struct{ Title, Body string }
			ThreadID          string `json:"thread-id"`
			Category          string
			InterruptionLevel string `json:"interruption-level"`
		}
		Mainloop map[string]string
	}
	if err := json.Unmarshal(sent[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.APS.Alert.Title != "Ship push" || p.APS.Alert.Body != "Task completed" || p.APS.ThreadID != "task:t1" ||
		p.APS.Category != apns.CategoryTask || p.APS.InterruptionLevel != "active" {
		t.Fatalf("aps = %+v", p.APS)
	}
	if want := map[string]string{"type": "task", "task_id": "t1", "href": "/tasks/t1"}; !equal(p.Mainloop, want) {
		t.Fatalf("mainloop = %v", p.Mainloop)
	}
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
