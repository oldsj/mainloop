package mcpserver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/ci"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/issues"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
)

const checkRunsHeadSHA = "1111111111111111111111111111111111111111"

type issuesHarness struct {
	*harness
	fake *githubtest.Server
	pool *pgxpool.Pool
}

// newIssuesHarness serves the issue and Dependabot-alert tools over a fake GitHub with one
// repository, reached through the same [github.App] construction the pull-request tools use.
func newIssuesHarness(t *testing.T) *issuesHarness {
	t.Helper()
	fake := githubtest.New(t)
	fake.AddRepo(githubtest.Repository{
		Owner: "o", Name: "n", ID: 7,
		IssueList: []githubtest.Issue{
			{Number: 5, Title: "Bug", Body: "It breaks", State: "open", Labels: []string{"bug"}},
		},
		DependabotAlerts: []githubtest.DependabotAlert{
			{Number: 1, PackageName: "lodash", Ecosystem: "npm", Severity: "high", VulnerableVersionRange: "< 4.17.21", PatchedVersion: "4.17.21", GHSAID: "GHSA-xxxx", Summary: "Prototype pollution"},
		},
		PullRequests: []githubtest.PullRequest{{Number: 42, Title: "T", HeadRef: "feature", HeadSHA: checkRunsHeadSHA}},
		Runs: map[string][]githubtest.CheckRun{
			checkRunsHeadSHA: {
				{ID: 1, AppID: 9, Name: "build", Conclusion: "success"},
				{ID: 2, AppID: 9, Name: "test", Conclusion: "failure", OutputSummary: "it failed", OutputText: "FAIL TestFoo"},
			},
		},
		Annotations: map[int64][]githubtest.Annotation{
			2: {{Path: "x_test.go", StartLine: 7, EndLine: 7, Level: "failure", Message: "want 1, got 2"}},
		},
	})
	app, err := github.NewApp(github.Options{AppID: fmt.Sprint(githubtest.AppID), PrivateKey: githubtest.PrivateKey(), BaseURL: fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	h := &issuesHarness{fake: fake}
	h.harness = buildHarness(t, false, false, func(srv *mcpserver.Server, pool *pgxpool.Pool) {
		h.pool = pool
		srv.Issues = issues.NewService(pool, app)
		srv.CI = ci.NewService(pool, app)
	})
	return h
}

func (h *issuesHarness) seedAgent(t *testing.T) authtest.Seeded {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(), `INSERT INTO projects (id, user_id, owner, name, full_name, html_url, default_branch)
		VALUES ('p1', 'owner', 'o', 'n', 'o/n', 'https://x', 'main') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	s := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	if _, err := h.pool.Exec(context.Background(), `UPDATE sessions SET project_id = 'p1' WHERE id = $1`, s.SessionID); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGetIssueFetchesAndRejectsAMissingIssue(t *testing.T) {
	h := newIssuesHarness(t)
	a := h.seedAgent(t)

	_, r := h.call(ordinary, a.Token, "get_issue", map[string]any{"project_id": "p1", "issue_number": 5})
	sc := r.Result.StructuredContent
	if r.Result.IsError || sc["number"] != float64(5) || sc["title"] != "Bug" || sc["body"] != "It breaks" ||
		sc["state"] != "open" {
		t.Fatalf("get_issue = %v %q", sc, r.text())
	}

	_, miss := h.call(ordinary, a.Token, "get_issue", map[string]any{"project_id": "p1", "issue_number": 999})
	if !miss.Result.IsError || miss.text() != "[not_found] no such issue" {
		t.Errorf("missing issue = %v %q", miss.Result.IsError, miss.text())
	}
}

func TestListDependabotAlertsFetches(t *testing.T) {
	h := newIssuesHarness(t)
	a := h.seedAgent(t)

	_, r := h.call(ordinary, a.Token, "list_dependabot_alerts", map[string]any{"project_id": "p1"})
	if r.Result.IsError {
		t.Fatalf("list_dependabot_alerts = %q", r.text())
	}
	alerts, _ := r.Result.StructuredContent["alerts"].([]any)
	if len(alerts) != 1 {
		t.Fatalf("alerts = %v", r.Result.StructuredContent)
	}
	first, _ := alerts[0].(map[string]any)
	if first["package"] != "lodash" || first["patched_version"] != "4.17.21" {
		t.Errorf("first alert = %v", first)
	}
}

func TestListDependabotAlertsNoneOpenIsAnEmptyList(t *testing.T) {
	h := newIssuesHarness(t)
	a := h.seedAgent(t)
	h.fake.AddRepo(githubtest.Repository{Owner: "o", Name: "n", ID: 7})

	_, r := h.call(ordinary, a.Token, "list_dependabot_alerts", map[string]any{"project_id": "p1"})
	if r.Result.IsError {
		t.Fatalf("list_dependabot_alerts = %q", r.text())
	}
	alerts, _ := r.Result.StructuredContent["alerts"].([]any)
	if len(alerts) != 0 {
		t.Errorf("alerts = %v, want none", alerts)
	}
}

func TestIssueToolsFailClosedWithoutProjectAuthority(t *testing.T) {
	h := newIssuesHarness(t)
	h.seedAgent(t)
	bare := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})

	if _, r := h.call(ordinary, bare.Token, "get_issue", map[string]any{"project_id": "p1", "issue_number": 5}); !r.Result.IsError || r.text()[:11] != "[forbidden]" {
		t.Errorf("get_issue without authority = %q", r.text())
	}
	if _, r := h.call(ordinary, bare.Token, "list_dependabot_alerts", map[string]any{"project_id": "p1"}); !r.Result.IsError || r.text()[:11] != "[forbidden]" {
		t.Errorf("list_dependabot_alerts without authority = %q", r.text())
	}
	if _, r := h.call(ordinary, bare.Token, "check_status", map[string]any{"project_id": "p1", "pr_number": 42}); !r.Result.IsError || r.text()[:11] != "[forbidden]" {
		t.Errorf("check_status without authority = %q", r.text())
	}
}

func TestIssueToolsAreUnavailableWithoutAGitHubApp(t *testing.T) {
	h := buildHarness(t, false, false, nil)
	a := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	if _, r := h.call(ordinary, a.Token, "get_issue", map[string]any{"project_id": "p1", "issue_number": 1}); !r.Result.IsError || r.text() != "[unavailable] issues are not available" {
		t.Errorf("get_issue with no service = %q", r.text())
	}
	if _, r := h.call(ordinary, a.Token, "list_dependabot_alerts", map[string]any{"project_id": "p1"}); !r.Result.IsError || r.text() != "[unavailable] issues are not available" {
		t.Errorf("list_dependabot_alerts with no service = %q", r.text())
	}
	if _, r := h.call(ordinary, a.Token, "check_status", map[string]any{"project_id": "p1", "pr_number": 1}); !r.Result.IsError || r.text() != "[unavailable] CI checks are not available" {
		t.Errorf("check_status with no service = %q", r.text())
	}
}

func TestCheckStatusReportsTheFailureWithItsOutputAndAnnotations(t *testing.T) {
	h := newIssuesHarness(t)
	a := h.seedAgent(t)

	_, r := h.call(ordinary, a.Token, "check_status", map[string]any{"project_id": "p1", "pr_number": 42})
	if r.Result.IsError {
		t.Fatalf("check_status = %q", r.text())
	}
	sc := r.Result.StructuredContent
	if sc["state"] != "failed" || sc["sha"] != checkRunsHeadSHA || sc["pr_number"] != float64(42) {
		t.Fatalf("status = %v", sc)
	}
	failures, _ := sc["failures"].([]any)
	if len(failures) != 1 {
		t.Fatalf("failures = %v", sc["failures"])
	}
	f, _ := failures[0].(map[string]any)
	ann, _ := f["annotations"].([]any)
	if f["name"] != "test" || f["summary"] != "it failed" || f["text"] != "FAIL TestFoo" || len(ann) != 1 {
		t.Errorf("failure = %v", f)
	}
	if !strings.Contains(r.text(), "CI failed at PR #42") || !strings.Contains(r.text(), "failure x_test.go:7 want 1, got 2") {
		t.Errorf("text = %q", r.text())
	}

	// The same commit by SHA, before or without a pull request.
	_, r = h.call(ordinary, a.Token, "check_status", map[string]any{"project_id": "p1", "commit_sha": checkRunsHeadSHA})
	if r.Result.IsError || r.Result.StructuredContent["state"] != "failed" {
		t.Errorf("by sha = %q", r.text())
	}
}

func TestCheckStatusRefusesAnUnknownPullRequestAndABadRef(t *testing.T) {
	h := newIssuesHarness(t)
	a := h.seedAgent(t)

	_, r := h.call(ordinary, a.Token, "check_status", map[string]any{"project_id": "p1", "pr_number": 999})
	if !r.Result.IsError || r.text() != "[not_found] no such pull request" {
		t.Errorf("unknown pull request = %v %q", r.Result.IsError, r.text())
	}
	for _, args := range []map[string]any{
		{"project_id": "p1"},
		{"project_id": "p1", "pr_number": 42, "commit_sha": checkRunsHeadSHA},
	} {
		if _, r := h.call(ordinary, a.Token, "check_status", args); !r.Result.IsError {
			t.Errorf("%v = %q, want a validation error", args, r.text())
		}
	}
}

func TestWatchChecksRecordsAWatchForTheCallingSessionAndReturnsAtOnce(t *testing.T) {
	h := newIssuesHarness(t)
	a := h.seedAgent(t)
	args := map[string]any{"project_id": "p1", "pr_number": 42, "request_id": "w1", "timeout_minutes": 10}

	_, r := h.call(ordinary, a.Token, "watch_checks", args)
	if r.Result.IsError {
		t.Fatalf("watch_checks = %q", r.text())
	}
	sc := r.Result.StructuredContent
	id, _ := sc["watch_id"].(string)
	if id == "" || sc["state"] != "watching" || !strings.Contains(r.text(), "End your turn now.") {
		t.Fatalf("watch = %v", sc)
	}
	var session, state string
	var pr int
	if err := h.env.Pool.QueryRow(context.Background(),
		`SELECT session_id, state, pr_number FROM check_watches WHERE id = $1`, id).Scan(&session, &state, &pr); err != nil {
		t.Fatal(err)
	}
	if session != a.SessionID || state != "watching" || pr != 42 {
		t.Errorf("row = %s %s %d", session, state, pr)
	}

	// A retry with the same request_id finds the same watch.
	if _, r := h.call(ordinary, a.Token, "watch_checks", args); r.Result.StructuredContent["watch_id"] != id {
		t.Errorf("retry = %v", r.Result.StructuredContent)
	}
	for _, bad := range []map[string]any{
		{"project_id": "p1", "pr_number": 42},
		{"project_id": "p1", "request_id": "w2"},
		{"project_id": "p1", "pr_number": 42, "request_id": "w3", "timeout_minutes": 500},
	} {
		if _, r := h.call(ordinary, a.Token, "watch_checks", bad); !r.Result.IsError {
			t.Errorf("%v = %q, want an error", bad, r.text())
		}
	}
}
