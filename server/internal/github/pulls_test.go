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
	if err != nil || d.ID != 7 || d.FullName != "o/n" || d.Archived {
		t.Fatalf("detail = %+v, %v", d, err)
	}
	m, err := app.MergeSettings(ctx, repoON)
	if err != nil || m.AllowSquashMerge == nil || *m.AllowSquashMerge {
		t.Fatalf("merge settings = %+v, %v", m, err)
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
	if p, err := app.BranchProtection(ctx, repoON, "main"); err != nil || !p.Present || !strings.Contains(string(p.Raw), `"ci"`) {
		t.Fatalf("protection = %+v, %v", p, err)
	}
	rules, err := app.BranchRules(ctx, repoON, "main")
	if err != nil || len(rules) != 1 || rules[0].Type != "required_status_checks" || !strings.Contains(string(rules[0].Parameters), `"ci"`) {
		t.Fatalf("rules = %+v, %v", rules, err)
	}
	// A 403 is the plan's refusal only with GitHub's exact message; any other 403 stays a status
	// error, so a permission failure can never be read as "no requirements".
	fake.Fail("GET", "/repos/o/n/rules/branches/main", http.StatusForbidden)
	_, err = app.BranchRules(ctx, repoON, "main")
	if st, ok := github.StatusOf(err); !ok || st != http.StatusForbidden || errors.Is(err, github.ErrPlanUnavailable) {
		t.Errorf("rules 403 = %d, %v (%v)", st, ok, err)
	}
}

func TestPlanRefusalNeedsTheExactMessageOnTheFirstPage(t *testing.T) {
	const plan = "Upgrade to GitHub Pro or make this repository public to enable this feature."
	for name, c := range map[string]struct {
		message string
		plan    bool
	}{
		"exact":      {plan, true},
		"permission": {"Resource not accessible by integration", false},
		"near miss":  {plan + " ", false},
		"empty":      {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			app, _ := pullApp(t, func(r *githubtest.Repository) {
				r.ProtectionStatus, r.ProtectionMessage = http.StatusForbidden, c.message
				r.RulesStatus, r.RulesMessage = http.StatusForbidden, c.message
			})
			ctx := context.Background()
			_, perr := app.BranchProtection(ctx, repoON, "main")
			_, rerr := app.BranchRules(ctx, repoON, "main")
			for _, err := range []error{perr, rerr} {
				if got := errors.Is(err, github.ErrPlanUnavailable); got != c.plan {
					t.Errorf("plan unavailable = %v, want %v (%v)", got, c.plan, err)
				}
				if st, ok := github.StatusOf(err); c.plan == (ok && st == http.StatusForbidden) {
					t.Errorf("status = %d, %v: a plan refusal must not look like a status error, nor the reverse", st, ok)
				}
			}
		})
	}
}

func TestPlanRefusalOnALaterRulesPageIsNotThePlan(t *testing.T) {
	app, _ := pullApp(t, func(r *githubtest.Repository) {
		for range 101 {
			r.Rules = append(r.Rules, githubtest.Rule{Type: "deletion"})
		}
		r.RulesLaterStatus = http.StatusForbidden
		r.RulesMessage = "Upgrade to GitHub Pro or make this repository public to enable this feature."
	})
	if _, err := app.BranchRules(context.Background(), repoON, "main"); err == nil || errors.Is(err, github.ErrPlanUnavailable) {
		t.Fatalf("a later page's plan message was taken for no rules: %v", err)
	}
}

func TestCompareReadsOnePageOfAComparison(t *testing.T) {
	const base = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	app, fake := pullApp(t, func(r *githubtest.Repository) {
		r.Branches["main"] = base
		r.PullRequests[0].Files = []githubtest.File{{Filename: "a.go", Additions: 2, Deletions: 1}, {Filename: "b.go", PreviousFilename: "c.go", Status: "renamed"}}
	})
	_ = fake
	ctx := context.Background()
	c, err := app.Compare(ctx, repoON, base, tipSHA, 100, 1)
	if err != nil || c.Status != "ahead" || c.AheadBy != 1 || c.BehindBy != 0 || c.BaseSHA != base || c.MergeBaseSHA != base ||
		len(c.Commits) != 1 || c.Commits[0].SHA != tipSHA || len(c.Files) != 2 || c.Files[1].PreviousFilename != "c.go" {
		t.Fatalf("comparison = %+v, %v", c, err)
	}
	for _, bad := range []struct {
		base, head string
		per, page  int
	}{
		{"main", tipSHA, 100, 1}, {base, "feature", 100, 1}, {base, tipSHA, 0, 1}, {base, tipSHA, 101, 1}, {base, tipSHA, 100, 0},
	} {
		if _, err := app.Compare(ctx, repoON, bad.base, bad.head, bad.per, bad.page); !errors.Is(err, github.ErrUnavailable) {
			t.Errorf("compare %+v = %v", bad, err)
		}
	}
	if _, err := app.Compare(ctx, repoON, "9999999999999999999999999999999999999999", tipSHA, 100, 1); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("unknown base = %v", err)
	}
}

func TestCheckReadsRefuseWhatIsNotThisCommitsEvidence(t *testing.T) {
	const other = "2222222222222222222222222222222222222222"
	ctx := context.Background()
	good := func(r *githubtest.Repository) {
		r.Suites = map[string][]githubtest.CheckSuite{tipSHA: {{ID: 1, AppID: 9, Conclusion: "success"}}}
		r.Runs = map[string][]githubtest.CheckRun{tipSHA: {{ID: 1, AppID: 9, Name: "ci", Conclusion: "success"}}}
	}
	app, _ := pullApp(t, good)
	if s, err := app.CheckSuites(ctx, repoON, tipSHA); err != nil || len(s) != 1 || s[0].AppID != 9 || s[0].HeadSHA != tipSHA {
		t.Fatalf("suites = %+v, %v", s, err)
	}
	if r, err := app.CheckRuns(ctx, repoON, tipSHA); err != nil || len(r) != 1 || r[0].SuiteID != 1 || r[0].HeadSHA != tipSHA {
		t.Fatalf("runs = %+v, %v", r, err)
	}
	for name, mut := range map[string]func(*githubtest.Repository){
		"run of another head":   func(r *githubtest.Repository) { r.Runs[tipSHA][0].HeadSHA = other },
		"suite of another head": func(r *githubtest.Repository) { r.Suites[tipSHA][0].HeadSHA = other },
		"repeated run id": func(r *githubtest.Repository) {
			r.Runs[tipSHA] = append(r.Runs[tipSHA], githubtest.CheckRun{ID: 1, AppID: 9, Name: "ci2", Conclusion: "success"})
		},
		"repeated suite id": func(r *githubtest.Repository) {
			r.Suites[tipSHA] = append(r.Suites[tipSHA], githubtest.CheckSuite{ID: 1, AppID: 9})
		},
	} {
		t.Run(name, func(t *testing.T) {
			app, _ := pullApp(t, func(r *githubtest.Repository) { good(r); mut(r) })
			_, errS := app.CheckSuites(ctx, repoON, tipSHA)
			_, errR := app.CheckRuns(ctx, repoON, tipSHA)
			if !errors.Is(errS, github.ErrUnavailable) && !errors.Is(errR, github.ErrUnavailable) {
				t.Fatalf("suites %v, runs %v", errS, errR)
			}
		})
	}
}
