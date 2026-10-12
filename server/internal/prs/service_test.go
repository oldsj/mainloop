package prs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/prs"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const (
	owner = "owner-1"
	tip   = "1111111111111111111111111111111111111111"
	other = "2222222222222222222222222222222222222222"
)

var pullsPath = "/repos/o/n/pulls"

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	fake *githubtest.Server
	svc  *prs.Service
	now  atomic.Int64 // offset added to the clock, in seconds
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	fake := githubtest.New(t)
	fake.AddRepo(githubtest.Repository{Owner: "o", Name: "n", ID: 7, Branches: map[string]string{"feature": tip}})
	app, err := github.NewApp(github.Options{AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, pool: d.Pool, fake: fake}
	e.svc = &prs.Service{
		Pool: d.Pool, GitHub: app, Log: quiet(), Settle: 5 * time.Second, Window: time.Minute,
		Recheck: 10 * time.Millisecond, Ceiling: 20 * time.Millisecond, Lease: time.Minute,
		Now: func() time.Time { return time.Now().Add(time.Duration(e.now.Load()) * time.Second) },
	}
	e.exec(`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('p1', $1, 'o', 'n', 'o/n', 'https://x')`, owner)
	e.exec(`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('foreign', 'someone', 'o', 'n2', 'o/n2', 'https://x')`)
	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

// run starts the creation loop until the test ends.
func (e *env) run() {
	e.t.Helper()
	r := reconcile.NewRunner(e.pool, quiet(), nil)
	l := e.svc.Loop()
	l.Interval = 10 * time.Millisecond
	r.Add(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	e.t.Cleanup(func() { cancel(); <-done })
}

func (e *env) input(req string) prs.OpenInput {
	return prs.OpenInput{OwnerID: owner, ProjectID: "p1", SessionID: "s1", Branch: "feature", ExpectedSHA: tip, Title: "T", Body: "B", RequestID: req}
}

func (e *env) state(id string) dbstate.PRCreationState {
	e.t.Helper()
	var s dbstate.PRCreationState
	if err := e.pool.QueryRow(context.Background(), `SELECT state FROM pr_creations WHERE id=$1`, id).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) posts() int { return e.fake.Count("POST", pullsPath) }

func (e *env) prs() int {
	n := 0
	for i := 1; i < 20; i++ {
		if _, ok := e.fake.PullRequestOf("o", "n", i); ok {
			n++
		}
	}
	return n
}

func TestOpenCreatesOnePullRequest(t *testing.T) {
	e := newEnv(t)
	e.run()
	res, err := e.svc.Open(context.Background(), e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != dbstate.PRCreated || res.Number != 1 || res.URL == "" || res.Repository != "o/n" || res.Head != "feature" || res.Base != "main" {
		t.Fatalf("res = %+v", res)
	}
	// The ledger row existed before the POST; the POST used the recorded values.
	if e.posts() != 1 {
		t.Fatalf("posts = %d", e.posts())
	}
	// The same call again finds the same row and sends nothing.
	again, err := e.svc.Open(context.Background(), e.input("r1"))
	if err != nil || again != res || e.posts() != 1 {
		t.Fatalf("replay = %+v, %v, posts %d", again, err, e.posts())
	}
	// A new request id for the same head, after it was created, does not make a second PR: the
	// open one is found by listing.
	second, err := e.svc.Open(context.Background(), e.input("r2"))
	if err != nil || second.State != dbstate.PRCreated || second.Number != 1 || e.posts() != 1 || e.prs() != 1 {
		t.Fatalf("second = %+v, %v, posts %d prs %d", second, err, e.posts(), e.prs())
	}
}

func TestOpenRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	in := e.input("r1")
	in.ProjectID = "foreign"
	if _, err := e.svc.Open(ctx, in); !errors.Is(err, prs.ErrNotFound) {
		t.Fatalf("foreign project = %v", err)
	}
	for name, mutate := range map[string]func(*prs.OpenInput){
		"default branch": func(i *prs.OpenInput) { i.Branch = "main" },
		"missing branch": func(i *prs.OpenInput) { i.Branch = "nope" },
		"moved branch":   func(i *prs.OpenInput) { i.ExpectedSHA = other },
		"bad name":       func(i *prs.OpenInput) { i.Branch = "../x" },
	} {
		in := e.input("r-" + name)
		mutate(&in)
		var r *prs.Refusal
		if _, err := e.svc.Open(ctx, in); !errors.As(err, &r) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM pr_creations`).Scan(&n)
	if n != 0 || e.posts() != 0 {
		t.Fatalf("rows %d posts %d: a refused call must write nothing", n, e.posts())
	}
}

func TestSameRequestIDWithOtherArgumentsConflicts(t *testing.T) {
	e := newEnv(t)
	e.run()
	if _, err := e.svc.Open(context.Background(), e.input("r1")); err != nil {
		t.Fatal(err)
	}
	in := e.input("r1")
	in.Title = "changed"
	if _, err := e.svc.Open(context.Background(), in); !errors.Is(err, prs.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

// A stop before the loop ran leaves a pending row; the replay and the loop make exactly one PR.
func TestReplayAfterStopAtPending(t *testing.T) {
	e := newEnv(t)
	e.svc.Settle = 0
	res, err := e.svc.Open(context.Background(), e.input("r1"))
	if err != nil || res.State != dbstate.PRPending || e.posts() != 0 {
		t.Fatalf("res = %+v, %v, posts %d", res, err, e.posts())
	}
	e.svc.Settle = 5 * time.Second
	e.run()
	again, err := e.svc.Open(context.Background(), e.input("r1"))
	if err != nil || again.State != dbstate.PRCreated || e.posts() != 1 || e.prs() != 1 {
		t.Fatalf("again = %+v, %v, posts %d prs %d", again, err, e.posts(), e.prs())
	}
}

// A stop after the claim marked the row dispatching, before or after the POST: the loop only looks.
func TestReplayAfterStopAtDispatching(t *testing.T) {
	t.Run("the POST never ran", func(t *testing.T) {
		e := newEnv(t)
		e.svc.Settle = 0
		res, err := e.svc.Open(context.Background(), e.input("r1"))
		if err != nil {
			t.Fatal(err)
		}
		e.exec(`UPDATE pr_creations SET state='dispatching' WHERE id=$1`, res.ID)
		e.svc.Window = time.Hour
		e.run()
		time.Sleep(300 * time.Millisecond)
		// Nothing was found, and it may have been sent: the loop keeps looking and never posts.
		if e.posts() != 0 || e.state(res.ID) != dbstate.PRUncertain {
			t.Fatalf("posts %d state %s", e.posts(), e.state(res.ID))
		}
		// Once the window has passed with no PR, the creation is refused, not retried.
		e.now.Store(7200)
		waitState(t, e, res.ID, dbstate.PRRefused)
		if e.posts() != 0 {
			t.Fatal("posted")
		}
		got, err := e.svc.Get(context.Background(), owner, res.ID)
		if err != nil || got.Detail != "not_created" {
			t.Fatalf("got %+v %v", got, err)
		}
	})
	t.Run("the POST ran", func(t *testing.T) {
		e := newEnv(t)
		e.svc.Settle = 0
		res, err := e.svc.Open(context.Background(), e.input("r1"))
		if err != nil {
			t.Fatal(err)
		}
		e.exec(`UPDATE pr_creations SET state='dispatching' WHERE id=$1`, res.ID)
		e.fake.Update("o", "n", func(r *githubtest.Repository) {
			r.PullRequests = append(r.PullRequests, githubtest.PullRequest{Number: 4, Title: "T", HeadRef: "feature", HeadSHA: tip})
		})
		e.run()
		waitState(t, e, res.ID, dbstate.PRCreated)
		got, _ := e.svc.Get(context.Background(), owner, res.ID)
		if got.Number != 4 || e.posts() != 0 {
			t.Fatalf("got %+v posts %d", got, e.posts())
		}
	})
}

func waitState(t *testing.T, e *env, id string, want dbstate.PRCreationState) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if e.state(id) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("state = %s, want %s", e.state(id), want)
}

// The POST took effect and its reply was lost: the row is uncertain, the loop finds the PR by
// listing, and nothing is posted twice.
func TestLostPostReplyIsResolvedByListing(t *testing.T) {
	e := newEnv(t)
	e.fake.LoseReply("POST", pullsPath, 1)
	e.run()
	res, err := e.svc.Open(context.Background(), e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != dbstate.PRCreated || res.Number != 1 || e.posts() != 1 || e.prs() != 1 {
		t.Fatalf("res = %+v posts %d prs %d", res, e.posts(), e.prs())
	}
}

// A POST that did not take effect and lost its reply is not repeated either: the creation stays
// uncertain until the window closes.
func TestLostPostThatDidNothingIsNeverRepeated(t *testing.T) {
	e := newEnv(t)
	e.fake.Fail("POST", pullsPath, 502)
	e.run()
	e.svc.Settle = 0
	res, err := e.svc.Open(context.Background(), e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, e, res.ID, dbstate.PRUncertain)
	time.Sleep(200 * time.Millisecond)
	if e.posts() != 1 {
		t.Fatalf("posts = %d, want exactly one", e.posts())
	}
	e.now.Store(7200)
	waitState(t, e, res.ID, dbstate.PRRefused)
	if e.posts() != 1 {
		t.Fatalf("posts = %d after the window", e.posts())
	}
}

func TestGitHubRefusalIsFinal(t *testing.T) {
	e := newEnv(t)
	e.fake.Fail("POST", pullsPath, 422)
	e.run()
	res, err := e.svc.Open(context.Background(), e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != dbstate.PRRefused || res.Detail != "github_422" || e.posts() != 1 {
		t.Fatalf("res = %+v posts %d", res, e.posts())
	}
	if strings.Contains(res.Detail, githubtest.SecretMarker) {
		t.Fatal("upstream text leaked")
	}
}

// A rate-limited POST provably did not run, so the creation goes back to pending and is sent later.
func TestRateLimitedPostIsRetriedLater(t *testing.T) {
	e := newEnv(t)
	e.fake.RateLimit("POST", pullsPath)
	e.run()
	e.svc.Settle = 0
	res, err := e.svc.Open(context.Background(), e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.posts() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	e.fake.ClearRateLimit("POST", pullsPath)
	waitState(t, e, res.ID, dbstate.PRCreated)
	if e.prs() != 1 {
		t.Fatalf("prs = %d", e.prs())
	}
}

// Two requests for one head, in flight together, are one creation.
func TestOneCreationPerHead(t *testing.T) {
	e := newEnv(t)
	e.svc.Settle = 0
	a, err := e.svc.Open(context.Background(), e.input("a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.svc.Open(context.Background(), e.input("b"))
	if err != nil || a.ID != b.ID {
		t.Fatalf("a=%+v b=%+v err=%v", a, b, err)
	}
	e.svc.Settle = 5 * time.Second
	e.run()
	waitState(t, e, a.ID, dbstate.PRCreated)
	if e.posts() != 1 {
		t.Fatalf("posts = %d", e.posts())
	}
}

func TestGetIsOwnerScoped(t *testing.T) {
	e := newEnv(t)
	e.svc.Settle = 0
	res, err := e.svc.Open(context.Background(), e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Get(context.Background(), "someone", res.ID); !errors.Is(err, prs.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestReviewMovedBranchMustNotCreateWrongHeadPR(t *testing.T) {
	// The intent was recorded for one repository, base and head commit. If any changed before the
	// POST was dispatched, the creation is refused and nothing is sent.
	cases := map[string]struct {
		change func(r *githubtest.Repository)
		detail string
	}{
		"branch moved":   {func(r *githubtest.Repository) { r.Branches["feature"] = other }, "branch_moved"},
		"branch deleted": {func(r *githubtest.Repository) { delete(r.Branches, "feature") }, "branch_missing"},
		"default branch changed": {func(r *githubtest.Repository) {
			r.Branches["trunk"] = tip
			r.DefaultBranch = "trunk"
		}, "default_branch_moved"},
		"repository replaced": {func(r *githubtest.Repository) { r.ID = 8 }, "repository_mismatch"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.svc.Settle = 0
			res, err := e.svc.Open(context.Background(), e.input("r1"))
			if err != nil || res.State != dbstate.PRPending {
				t.Fatalf("res = %+v, %v", res, err)
			}
			e.fake.Update("o", "n", tc.change)
			e.svc.Settle = 5 * time.Second
			e.run()
			got, err := e.svc.Open(context.Background(), e.input("r1"))
			if err != nil || got.State != dbstate.PRRefused || got.Detail != tc.detail || e.posts() != 0 || e.prs() != 0 {
				t.Fatalf("got = %+v, %v, posts %d prs %d", got, err, e.posts(), e.prs())
			}
		})
	}
}

func TestPreDispatchReadFailureIsRetriedNotRefused(t *testing.T) {
	e := newEnv(t)
	e.svc.Settle = 0
	res, err := e.svc.Open(context.Background(), e.input("r1"))
	if err != nil {
		t.Fatal(err)
	}
	e.fake.Fail("GET", "/repos/o/n", http.StatusBadGateway)
	e.run()
	time.Sleep(300 * time.Millisecond)
	if st := e.state(res.ID); st != dbstate.PRPending || e.posts() != 0 {
		t.Fatalf("state = %s, posts %d after a failed read", st, e.posts())
	}
	e.fake.Fail("GET", "/repos/o/n", 0)
	e.svc.Settle = 10 * time.Second
	got, err := e.svc.Open(context.Background(), e.input("r1"))
	if err != nil || got.State != dbstate.PRCreated || e.posts() != 1 {
		t.Fatalf("got = %+v, %v, posts %d", got, err, e.posts())
	}
}

// A bare Service returns at once, so the constructor the processes use must set the wait the spec
// promises; without it open_pull_request answers "pending" before the loop has run.
func TestNewServiceWaitsForTheLoopToSettle(t *testing.T) {
	if got := prs.NewService(nil, nil, nil).Settle; got != prs.DefaultSettle || got < 10*time.Second {
		t.Fatalf("Settle = %v, want %v", got, prs.DefaultSettle)
	}
}
