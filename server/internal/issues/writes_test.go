package issues_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/issues"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

const (
	issuesPath   = "/repos/o/n/issues"
	commentsPath = "/repos/o/n/issues/5/comments"
)

type writeEnv struct {
	*env
	now atomic.Int64 // offset added to the clock, in seconds
}

// newWriteEnv is newEnv with a write service whose loop is fast and whose clock a test can move.
func newWriteEnv(t *testing.T) *writeEnv {
	t.Helper()
	e := &writeEnv{env: newEnv(t)}
	e.fake.Update("o", "n", func(r *githubtest.Repository) { r.Labels = []string{"bug", "p1", "good first issue"} })
	e.svc = &issues.Service{
		Pool: e.pool, GitHub: e.app, Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Settle: 5 * time.Second,
		Window: time.Minute, Recheck: 10 * time.Millisecond, Ceiling: 20 * time.Millisecond, Lease: time.Minute,
		Now: func() time.Time { return time.Now().Add(time.Duration(e.now.Load()) * time.Second) },
	}
	return e
}

// run starts the write loop until the test ends.
func (e *writeEnv) run() {
	e.t.Helper()
	r := reconcile.NewRunner(e.pool, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	l := e.svc.Loop()
	l.Interval = 10 * time.Millisecond
	r.Add(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	e.t.Cleanup(func() { cancel(); <-done })
}

func (e *writeEnv) create(req string, labels ...string) issues.CreateInput {
	return issues.CreateInput{OwnerID: owner, ProjectID: "p1", SessionID: "s1", RequestID: req, Title: "Flaky test", Body: "It fails on Tuesdays", Labels: labels}
}

func (e *writeEnv) comment(req string, number int) issues.CommentInput {
	return issues.CommentInput{OwnerID: owner, ProjectID: "p1", SessionID: "s1", RequestID: req, Number: number, Body: "Found the cause"}
}

func (e *writeEnv) state(id string) dbstate.IssueWriteState {
	e.t.Helper()
	var s dbstate.IssueWriteState
	if err := e.pool.QueryRow(context.Background(), `SELECT state FROM issue_writes WHERE id=$1`, id).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *writeEnv) waitState(id string, want dbstate.IssueWriteState) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if e.state(id) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("state = %s, want %s", e.state(id), want)
}

func (e *writeEnv) detail(id string) string {
	e.t.Helper()
	var d *string
	if err := e.pool.QueryRow(context.Background(), `SELECT detail FROM issue_writes WHERE id=$1`, id).Scan(&d); err != nil {
		e.t.Fatal(err)
	}
	if d == nil {
		return ""
	}
	return *d
}

// issues returns the fake repository's issues.
func (e *writeEnv) issues() []githubtest.Issue {
	var out []githubtest.Issue
	e.fake.Update("o", "n", func(r *githubtest.Repository) { out = append(out, r.IssueList...) })
	return out
}

func (e *writeEnv) issue(n int) githubtest.Issue {
	e.t.Helper()
	for _, i := range e.issues() {
		if i.Number == n {
			return i
		}
	}
	e.t.Fatalf("no issue #%d", n)
	return githubtest.Issue{}
}

func TestCreateIssueFilesOnce(t *testing.T) {
	e := newWriteEnv(t)
	e.run()
	ctx := context.Background()
	res, err := e.svc.Create(ctx, e.create("r1", "BUG", "bug"))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != dbstate.IssueWriteCreated || res.Kind != dbstate.IssueWriteIssue || res.Number <= 0 || res.URL == "" || res.Repository != "o/n" {
		t.Fatalf("res = %+v", res)
	}
	got := e.issue(int(res.Number))
	// Labels are sent as the repository spells them, once; the body carries the agent's text and
	// the marker line.
	if got.Title != "Flaky test" || len(got.Labels) != 1 || got.Labels[0] != "bug" ||
		!strings.HasPrefix(got.Body, "It fails on Tuesdays\n\n<!-- mainloop:issue-write "+res.ID+" -->") {
		t.Fatalf("issue = %+v", got)
	}
	if n := e.fake.Count("POST", issuesPath); n != 1 {
		t.Fatalf("posts = %d", n)
	}
	// The same call again finds the same row and sends nothing.
	again, err := e.svc.Create(ctx, e.create("r1", "BUG", "bug"))
	if err != nil || again != res || e.fake.Count("POST", issuesPath) != 1 {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	// The same request id with other arguments is a conflict.
	if _, err := e.svc.Create(ctx, e.create("r1")); !errors.Is(err, issues.ErrConflict) {
		t.Fatalf("conflict = %v", err)
	}
	// A comment under a request id an issue already used conflicts too: the id is per owner.
	if _, err := e.svc.Comment(ctx, e.comment("r1", 5)); !errors.Is(err, issues.ErrConflict) {
		t.Fatalf("comment under an issue's request id = %v", err)
	}
}

func TestCreateIssueRefusalsWriteNothing(t *testing.T) {
	e := newWriteEnv(t)
	e.run()
	ctx := context.Background()
	var r *issues.Refusal
	if _, err := e.svc.Create(ctx, e.create("r1", "nonsense")); !errors.As(err, &r) || r.Code != "unknown_label" {
		t.Fatalf("unknown label = %v", err)
	}
	in := e.create("r2")
	in.ProjectID = "foreign"
	if _, err := e.svc.Create(ctx, in); !errors.Is(err, issues.ErrNotFound) {
		t.Fatalf("foreign project = %v", err)
	}
	e.fake.Fail("GET", "/repos/o/n", 502)
	if _, err := e.svc.Create(ctx, e.create("r3")); !errors.As(err, &r) || r.Code != "github_unavailable" {
		t.Fatalf("unreadable repository = %v", err)
	}
	var rows int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM issue_writes`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows = %d, %v", rows, err)
	}
	if n := e.fake.Count("POST", issuesPath); n != 0 {
		t.Fatalf("posts = %d", n)
	}
}

func TestCommentIssue(t *testing.T) {
	e := newWriteEnv(t)
	e.run()
	ctx := context.Background()
	res, err := e.svc.Comment(ctx, e.comment("c1", 5))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != dbstate.IssueWriteCreated || res.Kind != dbstate.IssueWriteComment || res.Number != 5 || res.CommentID <= 0 ||
		!strings.Contains(res.URL, "/issues/5#issuecomment-") {
		t.Fatalf("res = %+v", res)
	}
	if c := e.issue(5).Comments; len(c) != 1 || !strings.HasPrefix(c[0].Body, "Found the cause\n\n<!-- mainloop:issue-write ") {
		t.Fatalf("comments = %+v", c)
	}
	again, err := e.svc.Comment(ctx, e.comment("c1", 5))
	if err != nil || again != res || e.fake.Count("POST", commentsPath) != 1 {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	// A pull request's number, or one that names nothing, is refused before anything is written.
	var r *issues.Refusal
	for _, n := range []int{42, 999} {
		if _, err := e.svc.Comment(ctx, e.comment("c-"+string(rune('a'+n%26)), n)); !errors.As(err, &r) || r.Code != "not_found" {
			t.Fatalf("comment on #%d = %v", n, err)
		}
	}
}

// The POST took effect and its reply was lost: the loop finds the issue by its marker and nothing
// is posted twice.
func TestLostIssueReplyIsFoundByMarker(t *testing.T) {
	e := newWriteEnv(t)
	e.fake.LoseReply("POST", issuesPath, 1)
	e.run()
	before := len(e.issues())
	res, err := e.svc.Create(context.Background(), e.create("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != dbstate.IssueWriteCreated || res.Number <= 0 || e.fake.Count("POST", issuesPath) != 1 || len(e.issues()) != before+1 {
		t.Fatalf("res = %+v posts %d issues %d", res, e.fake.Count("POST", issuesPath), len(e.issues()))
	}
}

func TestLostCommentReplyIsFoundByMarker(t *testing.T) {
	e := newWriteEnv(t)
	e.fake.LoseReply("POST", commentsPath, 1)
	e.run()
	res, err := e.svc.Comment(context.Background(), e.comment("c1", 5))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != dbstate.IssueWriteCreated || res.CommentID <= 0 || e.fake.Count("POST", commentsPath) != 1 || len(e.issue(5).Comments) != 1 {
		t.Fatalf("res = %+v posts %d", res, e.fake.Count("POST", commentsPath))
	}
}

// A POST whose outcome is unknown and that did nothing is never repeated: the write stays
// uncertain until the window closes, then is refused.
func TestUnknownPostIsNeverRepeated(t *testing.T) {
	e := newWriteEnv(t)
	e.fake.Fail("POST", issuesPath, 502)
	e.svc.Settle = 0
	e.run()
	res, err := e.svc.Create(context.Background(), e.create("r1"))
	if err != nil {
		t.Fatal(err)
	}
	e.waitState(res.ID, dbstate.IssueWriteUncertain)
	time.Sleep(200 * time.Millisecond)
	if n := e.fake.Count("POST", issuesPath); n != 1 {
		t.Fatalf("posts = %d, want exactly one", n)
	}
	e.now.Store(7200)
	e.waitState(res.ID, dbstate.IssueWriteRefused)
	if n := e.fake.Count("POST", issuesPath); n != 1 || e.detail(res.ID) != "not_created" {
		t.Fatalf("posts = %d detail %q after the window", n, e.detail(res.ID))
	}
}

func TestGitHubRefusalIsFinal(t *testing.T) {
	e := newWriteEnv(t)
	e.fake.Fail("POST", issuesPath, 403)
	e.run()
	res, err := e.svc.Create(context.Background(), e.create("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != dbstate.IssueWriteRefused || res.Detail != "github_403" || e.fake.Count("POST", issuesPath) != 1 {
		t.Fatalf("res = %+v", res)
	}
}

// Only the App's own write counts as found: an issue by a person that quotes the marker does not
// stop the one POST.
func TestMarkerFromAPersonIsNotTheWrite(t *testing.T) {
	e := newWriteEnv(t)
	e.svc.Settle = 0
	res, err := e.svc.Create(context.Background(), e.create("r1"))
	if err != nil {
		t.Fatal(err)
	}
	e.fake.Update("o", "n", func(r *githubtest.Repository) {
		r.IssueList = append(r.IssueList, githubtest.Issue{
			Number: 90, Title: "copy", Body: "<!-- mainloop:issue-write " + res.ID + " -->", CreatedAt: time.Now().UTC(), UserType: "User",
		})
	})
	e.run()
	e.waitState(res.ID, dbstate.IssueWriteCreated)
	var number int64
	if err := e.pool.QueryRow(context.Background(), `SELECT issue_number FROM issue_writes WHERE id=$1`, res.ID).Scan(&number); err != nil {
		t.Fatal(err)
	}
	if number == 90 || e.fake.Count("POST", issuesPath) != 1 {
		t.Fatalf("issue %d posts %d", number, e.fake.Count("POST", issuesPath))
	}
}

// A comment's issue that became unreadable as an issue before the POST is refused, not posted.
func TestCommentTargetGoneBeforePostIsRefused(t *testing.T) {
	e := newWriteEnv(t)
	e.svc.Settle = 0
	res, err := e.svc.Comment(context.Background(), e.comment("c1", 5))
	if err != nil {
		t.Fatal(err)
	}
	e.fake.Update("o", "n", func(r *githubtest.Repository) { r.IssueList = nil })
	e.run()
	e.waitState(res.ID, dbstate.IssueWriteRefused)
	if e.detail(res.ID) != "issue_missing" || e.fake.Count("POST", commentsPath) != 0 {
		t.Fatalf("detail %q posts %d", e.detail(res.ID), e.fake.Count("POST", commentsPath))
	}
}
