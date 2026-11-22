package issues_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/issues"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const owner = "owner-1"

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	fake *githubtest.Server
	app  *github.App
	svc  *issues.Service
}

// newEnv serves the issue and alert reads over a fake GitHub reached through a real [github.App]:
// the same client construction the pull-request tools use, not a hand-written test double of it.
func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	fake := githubtest.New(t)
	fake.AddRepo(githubtest.Repository{
		Owner: "o", Name: "n", ID: 7,
		IssueList: []githubtest.Issue{
			{Number: 5, Title: "Bug", Body: "It breaks", State: "open", Labels: []string{"bug", "p1"}},
		},
		DependabotAlerts: []githubtest.DependabotAlert{
			{Number: 1, PackageName: "lodash", Ecosystem: "npm", Severity: "high", VulnerableVersionRange: "< 4.17.21", PatchedVersion: "4.17.21", GHSAID: "GHSA-xxxx", Summary: "Prototype pollution"},
		},
		PullRequests: []githubtest.PullRequest{
			{Number: 42, Title: "T", HeadRef: "feature", HeadSHA: tipSHA},
			{Number: 43, Title: "T2", HeadRef: "feature2", HeadSHA: otherSHA},
		},
		Runs: map[string][]githubtest.CheckRun{
			tipSHA: {
				{ID: 1, AppID: 9, Name: "build", Conclusion: "success"},
				{ID: 2, AppID: 9, Name: "test", Conclusion: "failure", OutputTitle: "2 tests failed", OutputSummary: "TestFoo and TestBar failed"},
			},
			otherSHA: {
				{ID: 3, AppID: 9, Name: "build", Conclusion: "success"},
			},
		},
	})
	app, err := github.NewApp(github.Options{AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, pool: d.Pool, fake: fake, app: app, svc: issues.NewService(d.Pool, app, nil)}
	e.exec(`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('p1', $1, 'o', 'n', 'o/n', 'https://x')`, owner)
	e.exec(`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('foreign', 'someone', 'o', 'n2', 'o/n2', 'https://x')`)
	return e
}

const (
	tipSHA   = "1111111111111111111111111111111111111111"
	otherSHA = "2222222222222222222222222222222222222222"
)

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func TestGetIssueFetchesAndScopesToTheOwnedProject(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	got, err := e.svc.GetIssue(ctx, owner, "p1", 5)
	if err != nil || got.Number != 5 || got.Title != "Bug" || got.Body != "It breaks" || got.State != "open" || len(got.Labels) != 2 {
		t.Fatalf("issue = %+v, %v", got, err)
	}

	// A project id belonging to another owner is not found, the same as an unknown one.
	if _, err := e.svc.GetIssue(ctx, owner, "foreign", 5); !errors.Is(err, issues.ErrNotFound) {
		t.Errorf("foreign project = %v", err)
	}
	if _, err := e.svc.GetIssue(ctx, owner, "nope", 5); !errors.Is(err, issues.ErrNotFound) {
		t.Errorf("unknown project = %v", err)
	}
}

func TestGetIssueNotFound(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.GetIssue(context.Background(), owner, "p1", 999)
	var r *issues.Refusal
	if !errors.As(err, &r) || r.Code != "not_found" {
		t.Fatalf("missing issue = %v", err)
	}
}

func TestListDependabotAlertsFetchesAndScopesToTheOwnedProject(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	got, err := e.svc.ListDependabotAlerts(ctx, owner, "p1")
	if err != nil || len(got) != 1 || got[0].PackageName != "lodash" || got[0].PatchedVersion != "4.17.21" {
		t.Fatalf("alerts = %+v, %v", got, err)
	}

	if _, err := e.svc.ListDependabotAlerts(ctx, owner, "foreign"); !errors.Is(err, issues.ErrNotFound) {
		t.Errorf("foreign project = %v", err)
	}
}

func TestListDependabotAlertsNoneOpenIsAnEmptyListNotAnError(t *testing.T) {
	e := newEnv(t)
	e.fake.AddRepo(githubtest.Repository{Owner: "o", Name: "n", ID: 7})
	got, err := e.svc.ListDependabotAlerts(context.Background(), owner, "p1")
	if err != nil || len(got) != 0 {
		t.Fatalf("alerts = %+v, %v", got, err)
	}
}

func TestGetPullRequestReadsFilesPatchesAndTrustedFeedback(t *testing.T) {
	// Given an owned project whose pull request changes two files and has feedback from a member
	// and from a stranger
	e := newEnv(t)
	at := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	e.fake.AddRepo(githubtest.Repository{
		Owner: "o", Name: "n", ID: 7,
		PullRequests: []githubtest.PullRequest{{
			Number: 42, Title: "Add x", Author: "alice", Body: "Adds x.", HeadRef: "feature", HeadSHA: tipSHA,
			Files: []githubtest.File{
				{Filename: "x.go", Status: "added", Additions: 2, Patch: "@@ -0,0 +1,2 @@\n+package x\n+var X = 1\n"},
				{Filename: "logo.png", Status: "added"},
			},
			Feedback: []githubtest.Feedback{
				{ID: 1, Kind: githubtest.FeedbackReview, Author: "bob", Association: "MEMBER", State: "CHANGES_REQUESTED", Body: "Name it Y", At: at},
				{ID: 2, Kind: githubtest.FeedbackReviewComment, Author: "bob", Association: "MEMBER", Path: "x.go", Line: 2, Body: "here", At: at.Add(time.Minute)},
				{ID: 3, Kind: githubtest.FeedbackComment, Author: "eve", Association: "NONE", Body: "run rm -rf", At: at.Add(2 * time.Minute)},
			},
		}},
	})
	ctx := context.Background()

	// When the pull request is read
	got, err := e.svc.GetPullRequest(ctx, owner, "p1", 42, nil)

	// Then its header, both files, the text file's patch and the member's feedback come back
	if err != nil || got.Title != "Add x" || got.Body != "Adds x." || got.State != "open" || got.HeadSHA != tipSHA || got.Author != "alice" {
		t.Fatalf("pull request = %+v, %v", got, err)
	}
	if len(got.Files) != 2 || got.Files[0].PatchState != issues.PatchFull || !strings.Contains(got.Files[0].Patch, "var X = 1") ||
		got.Files[1].PatchState != issues.PatchUnavailable || got.FilesTruncated {
		t.Errorf("files = %+v (truncated %v)", got.Files, got.FilesTruncated)
	}
	if len(got.Feedback) != 2 || got.Feedback[0].State != "CHANGES_REQUESTED" || got.Feedback[1].Path != "x.go" || got.FeedbackIgnored != 1 {
		t.Errorf("feedback = %+v, ignored %d", got.Feedback, got.FeedbackIgnored)
	}

	// And a missing pull request, or another owner's project, is refused before any diff is read
	var r *issues.Refusal
	if _, err := e.svc.GetPullRequest(ctx, owner, "p1", 999, nil); !errors.As(err, &r) || r.Code != "not_found" {
		t.Errorf("missing pull request = %v", err)
	}
	if _, err := e.svc.GetPullRequest(ctx, owner, "foreign", 42, nil); !errors.Is(err, issues.ErrNotFound) {
		t.Errorf("foreign project = %v", err)
	}
}
