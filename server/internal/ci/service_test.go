package ci_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/ci"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const owner = "owner-1"

var (
	headSHA  = strings.Repeat("1", 40)
	greenSHA = strings.Repeat("2", 40)
	emptySHA = strings.Repeat("3", 40)
	manySHA  = strings.Repeat("4", 40)
)

func fakeRepo() githubtest.Repository {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var many []githubtest.CheckRun
	for i := range ci.MaxDetailed + 2 {
		many = append(many, githubtest.CheckRun{ID: int64(100 + i), AppID: 9, Name: fmt.Sprintf("job-%d", i), Conclusion: "failure"})
	}
	return githubtest.Repository{
		Owner: "o", Name: "n", ID: 7,
		PullRequests: []githubtest.PullRequest{{Number: 42, Title: "T", HeadRef: "feature", HeadSHA: headSHA}},
		Runs: map[string][]githubtest.CheckRun{
			headSHA: {
				{ID: 1, AppID: 9, AppSlug: "github-actions", Name: "build", Conclusion: "success"},
				// An earlier attempt of test failed; the latest attempt is still failing, with output.
				{ID: 2, AppID: 9, Name: "test", Conclusion: "failure", OutputTitle: "old"},
				{
					ID: 3, AppID: 9, AppSlug: "github-actions", Name: "test", Conclusion: "failure", HTMLURL: "https://gh/run/3",
					OutputTitle: "2 tests failed", OutputSummary: "TestFoo and TestBar", OutputText: "--- FAIL: TestFoo",
				},
				{ID: 4, AppID: 9, Name: "image", Status: "in_progress"},
			},
			greenSHA: {{ID: 5, AppID: 9, Name: "build", Conclusion: "success"}},
			manySHA:  many,
		},
		Statuses: map[string][]githubtest.Status{
			headSHA: {
				{ID: 1, Context: "deploy/preview", State: "pending", CreatedAt: t0},
				{ID: 2, Context: "deploy/preview", State: "error", Description: "preview failed", TargetURL: "https://ci/p", CreatedAt: t0.Add(time.Minute)},
			},
		},
		Annotations: map[int64][]githubtest.Annotation{
			3: {{Path: "x_test.go", StartLine: 12, EndLine: 12, Level: "failure", Title: "TestFoo", Message: "want 1, got 2"}},
		},
	}
}

func newService(t *testing.T) (*ci.Service, *githubtest.Server) {
	t.Helper()
	d := testdb.New(t)
	fake := githubtest.New(t)
	fake.AddRepo(fakeRepo())
	app, err := github.NewApp(github.Options{AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, p := range []struct{ id, user string }{{"p1", owner}, {"foreign", "someone-else"}} {
		if _, err := d.Pool.Exec(ctx, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url, default_branch)
			VALUES ($1, $2, 'o', 'n', 'o/n', 'https://x', 'main')`, p.id, p.user); err != nil {
			t.Fatal(err)
		}
	}
	return ci.NewService(d.Pool, app), fake
}

func TestStatusOfAPullRequestReadsItsHeadAndExplainsTheFailures(t *testing.T) {
	svc, _ := newService(t)
	st, err := svc.Status(context.Background(), owner, "p1", ci.Ref{PR: 42})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != ci.Red || st.SHA != headSHA || st.PR != 42 {
		t.Fatalf("status = %+v", st)
	}
	// build passed; test (latest attempt) and the latest deploy/preview status failed; image runs.
	if st.Counts != (ci.Counts{Passed: 1, Failed: 2, Pending: 1}) || len(st.Checks) != 4 {
		t.Fatalf("counts = %+v, checks = %+v", st.Counts, st.Checks)
	}
	if len(st.Failures) != 2 {
		t.Fatalf("failures = %+v", st.Failures)
	}
	byName := map[string]ci.Failure{}
	for _, f := range st.Failures {
		byName[f.Name] = f
	}
	test := byName["test"]
	if test.Summary != "2 tests failed\n\nTestFoo and TestBar" || test.Text != "--- FAIL: TestFoo" || test.URL != "https://gh/run/3" {
		t.Errorf("test failure = %+v", test)
	}
	if len(test.Annotations) != 1 || test.Annotations[0] != (ci.Annotation{Path: "x_test.go", StartLine: 12, EndLine: 12, Level: "failure", Title: "TestFoo", Message: "want 1, got 2"}) {
		t.Errorf("annotations = %+v", test.Annotations)
	}
	if p := byName["deploy/preview"]; p.Summary != "preview failed" || p.URL != "https://ci/p" || len(p.Annotations) != 0 {
		t.Errorf("status failure = %+v", p)
	}
	for _, c := range st.Checks {
		if c.Text != "" {
			t.Errorf("listing carries long text for %s", c.Name)
		}
	}
}

func TestStatusByCommitWorksWithoutAPullRequest(t *testing.T) {
	svc, fake := newService(t)
	ctx := context.Background()
	st, err := svc.Status(ctx, owner, "p1", ci.Ref{SHA: greenSHA})
	if err != nil || st.State != ci.Green || st.PR != 0 || len(st.Failures) != 0 {
		t.Fatalf("green = %+v, %v", st, err)
	}
	st, err = svc.Status(ctx, owner, "p1", ci.Ref{SHA: emptySHA})
	if err != nil || st.State != ci.NoChecks {
		t.Fatalf("empty = %+v, %v", st, err)
	}
	if n := fake.Count("GET", "/repos/o/n/pulls/42"); n != 0 {
		t.Errorf("a SHA read resolved a pull request %d times", n)
	}
}

func TestStatusDetailsOnlyTheFirstFailures(t *testing.T) {
	svc, _ := newService(t)
	st, err := svc.Status(context.Background(), owner, "p1", ci.Ref{SHA: manySHA})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Failures) != ci.MaxDetailed || st.FailuresOmitted != 2 || st.Counts.Failed != ci.MaxDetailed+2 {
		t.Errorf("failures %d, omitted %d, counts %+v", len(st.Failures), st.FailuresOmitted, st.Counts)
	}
}

func TestStatusRefusals(t *testing.T) {
	svc, fake := newService(t)
	ctx := context.Background()
	var r *ci.Refusal
	if _, err := svc.Status(ctx, owner, "p1", ci.Ref{PR: 999}); !errors.As(err, &r) || r.Code != "not_found" {
		t.Errorf("unknown pull request = %v", err)
	}
	if _, err := svc.Status(ctx, owner, "p1", ci.Ref{}); !errors.As(err, &r) || r.Code != "invalid_ref" {
		t.Errorf("empty ref = %v", err)
	}
	// Another owner's project is not found, the same as an unknown one, before any GitHub read.
	before := len(fake.Requests())
	for _, id := range []string{"foreign", "nope"} {
		if _, err := svc.Status(ctx, owner, id, ci.Ref{PR: 42}); !errors.Is(err, ci.ErrProjectNotFound) {
			t.Errorf("project %s = %v", id, err)
		}
	}
	if after := len(fake.Requests()); after != before {
		t.Errorf("a refused project made %d GitHub requests", after-before)
	}
	fake.Fail("GET", "/repos/o/n/commits/"+emptySHA+"/check-runs", 422)
	if _, err := svc.Status(ctx, owner, "p1", ci.Ref{SHA: emptySHA}); !errors.As(err, &r) || r.Code != "not_found" {
		t.Errorf("unknown commit = %v", err)
	}
	fake.Fail("GET", "/repos/o/n/commits/"+headSHA+"/check-runs", 502)
	if _, err := svc.Status(ctx, owner, "p1", ci.Ref{PR: 42}); !errors.As(err, &r) || r.Code != "ci_unavailable" {
		t.Errorf("GitHub down = %v", err)
	}
}
