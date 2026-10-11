package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
)

const tipSHA = "1111111111111111111111111111111111111111"

func pullApp(t *testing.T, mut func(*githubtest.Repository)) (*github.App, *githubtest.Server) {
	t.Helper()
	fake := githubtest.New(t)
	r := githubtest.Repository{
		Owner: "o", Name: "n", ID: 7, Branches: map[string]string{"feature": tipSHA},
		PullRequests: []githubtest.PullRequest{{Number: 1, Title: "T", HeadRef: "feature", HeadSHA: tipSHA}},
	}
	if mut != nil {
		mut(&r)
	}
	fake.AddRepo(r)
	app, err := github.NewApp(github.Options{AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	return app, fake
}

var repoON = github.Repo{Owner: "o", Name: "n"}

func TestRepoDetailAndBranchTip(t *testing.T) {
	app, _ := pullApp(t, func(r *githubtest.Repository) { r.NoSquash = true })
	ctx := context.Background()
	d, err := app.RepoDetail(ctx, repoON)
	if err != nil || d.ID != 7 || d.FullName != "o/n" || d.AllowSquashMerge || d.Archived {
		t.Fatalf("detail = %+v, %v", d, err)
	}
	if tip, err := app.BranchTip(ctx, repoON, "feature"); err != nil || tip != tipSHA {
		t.Fatalf("tip = %q, %v", tip, err)
	}
	if _, err := app.BranchTip(ctx, repoON, "gone"); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("missing branch = %v", err)
	}
	// A branch name that would need escaping is refused before any request.
	for _, bad := range []string{"", "/x", "x/", "a//b", "a/../b", "a b", "a%2Fb", "a?b"} {
		if _, err := app.BranchTip(ctx, repoON, bad); !errors.Is(err, github.ErrBranchName) {
			t.Errorf("branch %q = %v", bad, err)
		}
	}
}

func TestStatusOfNamesOnlyGitHubAnswers(t *testing.T) {
	app, fake := pullApp(t, nil)
	ctx := context.Background()
	if _, err := app.PullRequest(ctx, repoON, 99); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("missing PR = %v", err)
	}
	if st, ok := github.StatusOf(github.ErrNotFound); !ok || st != http.StatusNotFound {
		t.Errorf("StatusOf(ErrNotFound) = %d, %v", st, ok)
	}
	fake.Fail("GET", "/repos/o/n/pulls/1", http.StatusBadGateway)
	_, err := app.PullRequest(ctx, repoON, 1)
	if st, ok := github.StatusOf(err); !ok || st != http.StatusBadGateway {
		t.Errorf("502 = %d, %v (%v)", st, ok, err)
	}
	if _, ok := github.StatusOf(errors.New("connection reset")); ok {
		t.Error("a transport error carries a status")
	}
	// The error names the status and nothing of the response body.
	if err != nil && err.Error() != "github: upstream status 502" {
		t.Errorf("error leaks the upstream body: %v", err)
	}
}

func TestRateLimitIsA429NotA403(t *testing.T) {
	app, fake := pullApp(t, nil)
	fake.RateLimit("GET", "/repos/o/n/pulls/1")
	_, err := app.PullRequest(context.Background(), repoON, 1)
	if st, ok := github.StatusOf(err); !ok || st != http.StatusTooManyRequests {
		t.Fatalf("rate limited = %d, %v (%v)", st, ok, err)
	}
}

func TestPullRequestsForHeadFiltersAndCreateReturnsThePR(t *testing.T) {
	app, _ := pullApp(t, func(r *githubtest.Repository) {
		r.PullRequests = append(r.PullRequests, githubtest.PullRequest{Number: 2, Title: "Other", HeadRef: "other", HeadSHA: tipSHA, Closed: true})
	})
	ctx := context.Background()
	got, err := app.PullRequestsForHead(ctx, repoON, "feature", "main")
	if err != nil || len(got) != 1 || got[0].Number != 1 || got[0].Head.Ref != "feature" || got[0].Head.RepoFullName != "o/n" {
		t.Fatalf("list = %+v, %v", got, err)
	}
	closed, err := app.PullRequestsForHead(ctx, repoON, "other", "main")
	if err != nil || len(closed) != 1 || closed[0].State != "closed" {
		t.Fatalf("closed PRs must be listed too: %+v, %v", closed, err)
	}
	made, err := app.CreatePullRequest(ctx, repoON, "feature2", "main", "New", "body")
	if err == nil {
		// feature2 does not exist, so GitHub refuses with a 422.
		t.Fatalf("created %+v from a missing branch", made)
	}
	if st, ok := github.StatusOf(err); !ok || st != http.StatusUnprocessableEntity {
		t.Errorf("create from a missing branch = %d, %v (%v)", st, ok, err)
	}
}

func TestPullFilesPage(t *testing.T) {
	var files []githubtest.File
	for i := range 5 {
		files = append(files, githubtest.File{Filename: fmt.Sprintf("f%d", i), Additions: 1})
	}
	app, _ := pullApp(t, func(r *githubtest.Repository) { r.PullRequests[0].Files = files })
	ctx := context.Background()
	first, more, err := app.PullFiles(ctx, repoON, 1, 1, 3)
	if err != nil || len(first) != 3 || !more || first[0].Filename != "f0" {
		t.Fatalf("page 1 = %+v, %v, %v", first, more, err)
	}
	rest, more, err := app.PullFiles(ctx, repoON, 1, 2, 3)
	if err != nil || len(rest) != 2 || more {
		t.Fatalf("page 2 = %+v, %v, %v", rest, more, err)
	}
}

func TestMergeSendsSquashAtTheHeadAndRefusalsCarryTheirStatus(t *testing.T) {
	app, fake := pullApp(t, nil)
	ctx := context.Background()
	res, err := app.MergePullRequest(ctx, repoON, 1, tipSHA)
	if err != nil || !res.Merged || res.SHA == "" {
		t.Fatalf("merge = %+v, %v", res, err)
	}
	var sent string
	for _, r := range fake.Requests() {
		if r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/pulls/1/merge") {
			sent = r.Body
		}
	}
	if !strings.Contains(sent, `"merge_method":"squash"`) || !strings.Contains(sent, tipSHA) {
		t.Errorf("merge body = %s", sent)
	}

	app2, _ := pullApp(t, func(r *githubtest.Repository) { r.MergeStatus = http.StatusForbidden })
	_, err = app2.MergePullRequest(ctx, repoON, 1, tipSHA)
	if st, ok := github.StatusOf(err); !ok || st != http.StatusForbidden {
		t.Errorf("refused merge = %d, %v (%v)", st, ok, err)
	}
}

func TestCIAndRuleReadsReportTheirStatus(t *testing.T) {
	app, fake := pullApp(t, func(r *githubtest.Repository) {
		r.Runs = map[string][]githubtest.CheckRun{tipSHA: {{ID: 1, AppID: 9, Name: "ci", Conclusion: "success"}}}
		r.Statuses = map[string][]githubtest.Status{tipSHA: {{ID: 1, Context: "legacy", State: "success"}}}
		r.Protection = &githubtest.Protection{RequiredChecks: []string{"ci"}}
		r.Rules = []githubtest.Rule{{Type: "required_status_checks", RequiredChecks: []string{"ci"}}}
	})
	ctx := context.Background()
	if runs, err := app.CheckRuns(ctx, repoON, tipSHA); err != nil || len(runs) != 1 || runs[0].Name != "ci" || runs[0].Conclusion != "success" {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if sts, err := app.CommitStatuses(ctx, repoON, tipSHA); err != nil || len(sts) != 1 || sts[0].Context != "legacy" {
		t.Fatalf("statuses = %+v, %v", sts, err)
	}
	if p, err := app.BranchProtection(ctx, repoON, "main"); err != nil || p == nil {
		t.Fatalf("protection = %+v, %v", p, err)
	}
	if rules, err := app.BranchRules(ctx, repoON, "main"); err != nil || len(rules) != 1 || rules[0].Type != "required_status_checks" {
		t.Fatalf("rules = %+v, %v", rules, err)
	}
	// The status code, not the body, is what a caller sees for a plan-gated read.
	fake.Fail("GET", "/repos/o/n/rules/branches/main", http.StatusForbidden)
	_, err := app.BranchRules(ctx, repoON, "main")
	if st, ok := github.StatusOf(err); !ok || st != http.StatusForbidden {
		t.Errorf("rules 403 = %d, %v (%v)", st, ok, err)
	}
}
