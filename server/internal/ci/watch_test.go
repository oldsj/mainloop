package ci_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/ci"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

type sent struct {
	session, id, text string
	steer             bool
}

// fakeSender records deliveries. err, when set, is answered (and cleared) by the next send.
type fakeSender struct {
	mu    sync.Mutex
	sends []sent
	err   error
}

func (f *fakeSender) SendToWith(_ context.Context, _ sessions.Source, sessionID, messageID, text string, o sessions.SendOptions) (sessions.Sent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err; err != nil {
		f.err = nil
		return sessions.Sent{}, err
	}
	f.sends = append(f.sends, sent{sessionID, messageID, text, o.Steer})
	return sessions.Sent{MessageID: messageID}, nil
}

type watchEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	fake   *githubtest.Server
	svc    *ci.Service
	sender *fakeSender
	now    time.Time
}

func newWatchEnv(t *testing.T) *watchEnv {
	t.Helper()
	d := testdb.New(t)
	fake := githubtest.New(t)
	fake.AddRepo(fakeRepo())
	app, err := github.NewApp(github.Options{AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	e := &watchEnv{t: t, pool: d.Pool, fake: fake, sender: &fakeSender{}, now: time.Now()}
	e.svc = ci.NewService(d.Pool, app)
	e.svc.Sender = e.sender
	e.svc.Now = func() time.Time { return e.now }
	e.exec(`INSERT INTO projects (id, user_id, owner, name, full_name, html_url, default_branch)
		VALUES ('p1', $1, 'o', 'n', 'o/n', 'https://x', 'main')`, owner)
	e.exec(`INSERT INTO sessions (id, user_id, project_id, title, description, prompt) VALUES ('s1', $1, 'p1', 't', 'd', 'p')`, owner)
	return e
}

func (e *watchEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *watchEnv) start(ref ci.Ref, request string, timeout time.Duration) ci.Watch {
	e.t.Helper()
	w, err := e.svc.StartWatch(context.Background(), ci.WatchInput{
		OwnerID: owner, ProjectID: "p1", SessionID: "s1", Ref: ref, RequestID: request, Timeout: timeout,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

// step makes every watch due, claims them and runs the handler on each, as one pass of the loop.
// The handler's own outcome (a retry) is recorded by the runner, which this test does not run, so
// the previous pass's leases are released here the way the runner would.
func (e *watchEnv) step() {
	e.t.Helper()
	ctx := context.Background()
	e.exec(`UPDATE check_watches SET next_attempt_at = now() - interval '1 second', lease_owner = NULL, lease_until = NULL`)
	claims, err := ci.WatchTable().Claim(ctx, e.pool, "test", time.Minute, 10)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range claims {
		if _, err := e.svc.HandleWatch(ctx, c); err != nil && !errors.Is(err, sessions.ErrBusy) {
			e.t.Logf("handle %s: %v", c.ID, err)
		}
	}
}

func (e *watchEnv) row(id string) (state, outcome string) {
	e.t.Helper()
	var o *string
	if err := e.pool.QueryRow(context.Background(), `SELECT state, outcome FROM check_watches WHERE id = $1`, id).Scan(&state, &o); err != nil {
		e.t.Fatal(err)
	}
	if o != nil {
		outcome = *o
	}
	return state, outcome
}

func (e *watchEnv) sends() []sent {
	e.sender.mu.Lock()
	defer e.sender.mu.Unlock()
	return append([]sent(nil), e.sender.sends...)
}

func TestWatchDeliversOnceWhenNothingIsPending(t *testing.T) {
	e := newWatchEnv(t)
	w := e.start(ci.Ref{PR: 42}, "r1", 0)
	if w.State != "watching" || w.Deadline.Sub(e.now).Round(time.Millisecond) != ci.DefaultWatchTimeout {
		t.Fatalf("watch = %+v", w)
	}

	// The image job is still running: the watch keeps polling and sends nothing.
	e.step()
	if st, _ := e.row(w.ID); st != "watching" || len(e.sends()) != 0 {
		t.Fatalf("state %s, sends %v", st, e.sends())
	}

	// It finishes; the next poll settles failed (test and the preview status failed) and delivers.
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		runs := r.Runs[headSHA]
		runs[3].Status, runs[3].Conclusion = "completed", "success"
		r.Runs[headSHA] = runs
	})
	e.step()
	st, outcome := e.row(w.ID)
	got := e.sends()
	if st != "delivered" || outcome != "failed" || len(got) != 1 {
		t.Fatalf("state %s outcome %s sends %v", st, outcome, got)
	}
	m := got[0]
	if m.session != "s1" || m.id != w.ID || !m.steer {
		t.Errorf("send = %+v", m)
	}
	for _, want := range []string{"watch_checks result for PR #42", "CI failed at PR #42", "x_test.go:12 TestFoo: want 1, got 2", "call watch_checks again"} {
		if !strings.Contains(m.text, want) {
			t.Errorf("message lacks %q:\n%s", want, m.text)
		}
	}

	// A settled watch is not claimed again.
	e.step()
	if len(e.sends()) != 1 {
		t.Errorf("sent %d times", len(e.sends()))
	}
}

func TestWatchGreenCommit(t *testing.T) {
	e := newWatchEnv(t)
	w := e.start(ci.Ref{SHA: greenSHA}, "r1", 0)
	e.step()
	if st, outcome := e.row(w.ID); st != "delivered" || outcome != "green" {
		t.Fatalf("state %s outcome %s", st, outcome)
	}
	if got := e.sends(); len(got) != 1 || !strings.Contains(got[0].text, "Every check passed.") {
		t.Errorf("sends = %v", got)
	}
}

func TestWatchTimesOutWhilePending(t *testing.T) {
	e := newWatchEnv(t)
	w := e.start(ci.Ref{PR: 42}, "r1", 5*time.Minute)
	e.step()
	e.now = e.now.Add(6 * time.Minute)
	e.step()
	if st, outcome := e.row(w.ID); st != "delivered" || outcome != "pending_timeout" {
		t.Fatalf("state %s outcome %s", st, outcome)
	}
	if got := e.sends(); len(got) != 1 || !strings.Contains(got[0].text, "had not settled by the watch deadline") ||
		!strings.Contains(got[0].text, "pending image") {
		t.Errorf("sends = %v", got)
	}
}

func TestWatchWaitsOutTheNoChecksGrace(t *testing.T) {
	e := newWatchEnv(t)
	w := e.start(ci.Ref{SHA: emptySHA}, "r1", 0)
	e.step()
	if st, _ := e.row(w.ID); st != "watching" {
		t.Fatalf("no checks yet settled at once: %s", st)
	}
	e.now = e.now.Add(4 * time.Minute)
	e.step()
	if st, outcome := e.row(w.ID); st != "delivered" || outcome != "no_checks" {
		t.Fatalf("state %s outcome %s", st, outcome)
	}
}

func TestWatchCountsARunningSuiteWithoutRunsAsPending(t *testing.T) {
	e := newWatchEnv(t)
	// The build passed, but its workflow's suite is still in progress: a dependent job is coming.
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.Suites = map[string][]githubtest.CheckSuite{greenSHA: {{ID: 1, AppID: 9, Status: "in_progress"}}}
	})
	w := e.start(ci.Ref{SHA: greenSHA}, "r1", 0)
	e.step()
	if st, _ := e.row(w.ID); st != "watching" {
		t.Fatalf("settled while a suite ran: %s", st)
	}
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.Suites[greenSHA][0].Status, r.Suites[greenSHA][0].Conclusion = "completed", "success"
	})
	e.step()
	if st, outcome := e.row(w.ID); st != "delivered" || outcome != "green" {
		t.Fatalf("state %s outcome %s", st, outcome)
	}
}

func TestWatchUnknownPullRequestIsNotFound(t *testing.T) {
	e := newWatchEnv(t)
	w := e.start(ci.Ref{PR: 999}, "r1", 0)
	e.step()
	if st, outcome := e.row(w.ID); st != "delivered" || outcome != "not_found" {
		t.Fatalf("state %s outcome %s", st, outcome)
	}
}

func TestWatchRequestIDsAndSupersession(t *testing.T) {
	e := newWatchEnv(t)
	ctx := context.Background()
	first := e.start(ci.Ref{PR: 42}, "r1", 0)
	if again := e.start(ci.Ref{PR: 42}, "r1", 0); again.ID != first.ID {
		t.Errorf("retry made a second watch: %s vs %s", again.ID, first.ID)
	}
	_, err := e.svc.StartWatch(ctx, ci.WatchInput{OwnerID: owner, ProjectID: "p1", SessionID: "s1", Ref: ci.Ref{PR: 43}, RequestID: "r1"})
	var r *ci.Refusal
	if !errors.As(err, &r) || r.Code != "request_id_reused" {
		t.Errorf("reused request id = %v", err)
	}
	other := e.start(ci.Ref{SHA: greenSHA}, "r2", 0)
	second := e.start(ci.Ref{PR: 42}, "r3", 0)
	if second.Superseded != 1 {
		t.Errorf("superseded = %d", second.Superseded)
	}
	if st, _ := e.row(first.ID); st != "superseded" {
		t.Errorf("first = %s", st)
	}
	if st, _ := e.row(other.ID); st != "watching" {
		t.Errorf("a watch of another ref = %s", st)
	}
	for _, in := range []ci.WatchInput{
		{OwnerID: owner, ProjectID: "p1", SessionID: "s1", Ref: ci.Ref{PR: 42}, RequestID: "x", Timeout: 3 * time.Hour},
		{OwnerID: owner, ProjectID: "p1", SessionID: "s1", Ref: ci.Ref{}, RequestID: "x"},
	} {
		if _, err := e.svc.StartWatch(ctx, in); !errors.As(err, &r) {
			t.Errorf("%+v = %v, want a refusal", in, err)
		}
	}
	if _, err := e.svc.StartWatch(ctx, ci.WatchInput{OwnerID: "someone-else", ProjectID: "p1", SessionID: "s1", Ref: ci.Ref{PR: 42}, RequestID: "y"}); !errors.Is(err, ci.ErrProjectNotFound) {
		t.Errorf("foreign owner = %v", err)
	}
}

func TestWatchDeliveryRetriesABusySendAndGivesUpOnAGoneSession(t *testing.T) {
	e := newWatchEnv(t)
	w := e.start(ci.Ref{SHA: greenSHA}, "r1", 0)
	e.sender.err = sessions.ErrBusy
	e.step()
	if st, outcome := e.row(w.ID); st != "delivering" || outcome != "green" || len(e.sends()) != 0 {
		t.Fatalf("after a busy send: state %s outcome %s sends %v", st, outcome, e.sends())
	}
	e.step()
	if st, _ := e.row(w.ID); st != "delivered" || len(e.sends()) != 1 || e.sends()[0].id != w.ID {
		t.Fatalf("after retry: state %s sends %v", st, e.sends())
	}

	gone := e.start(ci.Ref{SHA: greenSHA}, "r2", 0)
	e.sender.err = sessions.ErrNotFound
	e.step()
	if st, _ := e.row(gone.ID); st != "undeliverable" {
		t.Errorf("gone session = %s", st)
	}
}
