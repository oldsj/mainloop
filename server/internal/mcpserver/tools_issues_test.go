package mcpserver_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/ci"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
	"github.com/oldsj/mainloop/server/internal/issues"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
	"github.com/oldsj/mainloop/server/internal/reconcile"
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
		PullRequests: []githubtest.PullRequest{
			{Number: 42, Title: "T", HeadRef: "feature", HeadSHA: checkRunsHeadSHA},
			// 44 is the pull request get_pull_request reads: a file with a patch and a member's review.
			{
				Number: 44, Title: "Add x", Author: "alice", Body: "Adds x.", HeadRef: "feature-x", HeadSHA: checkRunsHeadSHA,
				Files: []githubtest.File{{Filename: "x.go", Status: "added", Additions: 1, Patch: "@@ -0,0 +1 @@\n+package x\n"}},
				Feedback: []githubtest.Feedback{
					{ID: 1, Kind: githubtest.FeedbackReview, Author: "bob", Association: "MEMBER", State: "APPROVED", Body: "LGTM", At: time.Now().Add(-time.Hour)},
				},
			},
		},
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
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		srv.Issues = &issues.Service{
			Pool: pool, GitHub: app, Log: log, Settle: 5 * time.Second, Window: time.Minute,
			Recheck: 10 * time.Millisecond, Ceiling: 20 * time.Millisecond, Lease: time.Minute, Now: time.Now,
		}
		srv.CI = ci.NewService(pool, app)
		r := reconcile.NewRunner(pool, log, nil)
		l := srv.Issues.Loop()
		l.Interval = 10 * time.Millisecond
		r.Add(l)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = r.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
	})
	return h
}

// seedAs seeds an agent of role and grant bound to p1 (the main thread is bound to no project).
func (h *issuesHarness) seedAs(t *testing.T, role dbstate.BindingRole, grant dbstate.MCPGrantKind, parent string) authtest.Seeded {
	t.Helper()
	h.seedAgent(t)
	s := h.env.Seed(t, authtest.Agent{Role: role, Grant: grant, Parent: parent})
	if role != dbstate.RoleMain {
		if _, err := h.pool.Exec(context.Background(), `UPDATE sessions SET project_id = 'p1' WHERE id = $1`, s.SessionID); err != nil {
			t.Fatal(err)
		}
	}
	return s
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

func TestGetVerificationResolvesTheProjectsModeForTheAgent(t *testing.T) {
	h := newIssuesHarness(t)
	a := h.seedAgent(t)
	_, r := h.call(ordinary, a.Token, "get_verification", map[string]any{"project_id": "p1"})
	if r.Result.IsError {
		t.Fatalf("get_verification = %q", r.text())
	}
	sc := r.Result.StructuredContent
	if sc["mode"] != "auto" || sc["source"] != "detected" || sc["detection"] == nil || !strings.Contains(r.text(), "Verification: ") {
		t.Fatalf("verification = %v", sc)
	}
	if _, err := h.env.Pool.Exec(context.Background(), `UPDATE projects SET verification_mode = 'ci' WHERE id = 'p1'`); err != nil {
		t.Fatal(err)
	}
	_, r = h.call(ordinary, a.Token, "get_verification", map[string]any{"project_id": "p1"})
	if sc := r.Result.StructuredContent; sc["effective_mode"] != "ci" || sc["source"] != "owner" || !strings.Contains(r.text(), "watch_checks") {
		t.Errorf("owner mode = %v", sc)
	}
}

func TestCreateAndCommentIssueFromACoordinationTask(t *testing.T) {
	h := newIssuesHarness(t)
	main := h.seedAs(t, dbstate.RoleMain, dbstate.GrantCoordination, "")
	sup := h.seedAs(t, dbstate.RoleSupervisor, dbstate.GrantCoordination, main.SessionID)
	h.fake.Update("o", "n", func(r *githubtest.Repository) { r.Labels = []string{"bug"} })

	_, r := h.call(ordinary, sup.Token, "create_issue", map[string]any{
		"project_id": "p1", "title": "Flaky test", "body": "It flakes.", "labels": []string{"Bug"}, "request_id": "r1",
	})
	sc := r.Result.StructuredContent
	if r.Result.IsError || sc["state"] != "created" || sc["kind"] != "issue" || sc["repository"] != "o/n" || sc["issue_number"] == nil ||
		!strings.HasPrefix(fmt.Sprint(sc["url"]), "https://github.com/o/n/issues/") {
		t.Fatalf("create_issue = %v %q", sc, r.text())
	}
	// A retry with the same request_id answers the same issue and files nothing.
	_, again := h.call(ordinary, sup.Token, "create_issue", map[string]any{
		"project_id": "p1", "title": "Flaky test", "body": "It flakes.", "labels": []string{"Bug"}, "request_id": "r1",
	})
	if again.Result.StructuredContent["url"] != sc["url"] || h.fake.Count("POST", "/repos/o/n/issues") != 1 {
		t.Fatalf("replay = %v, posts %d", again.Result.StructuredContent, h.fake.Count("POST", "/repos/o/n/issues"))
	}
	_, conflict := h.call(ordinary, sup.Token, "create_issue", map[string]any{"project_id": "p1", "title": "Other", "body": "", "request_id": "r1"})
	if conflict.text() != "[request_payload_conflict] this request_id was used with other arguments" {
		t.Errorf("conflict = %q", conflict.text())
	}
	_, unknown := h.call(ordinary, sup.Token, "create_issue", map[string]any{"project_id": "p1", "title": "T", "body": "", "labels": []string{"nope"}, "request_id": "r2"})
	if !unknown.Result.IsError || !strings.HasPrefix(unknown.text(), "[unknown_label]") {
		t.Errorf("unknown label = %q", unknown.text())
	}

	_, c := h.call(ordinary, sup.Token, "comment_issue", map[string]any{"project_id": "p1", "issue_number": 5, "body": "Root cause: a race.", "request_id": "c1"})
	if c.Result.IsError || c.Result.StructuredContent["state"] != "created" || c.Result.StructuredContent["comment_id"] == nil ||
		!strings.Contains(fmt.Sprint(c.Result.StructuredContent["url"]), "/issues/5#issuecomment-") {
		t.Fatalf("comment_issue = %v %q", c.Result.StructuredContent, c.text())
	}
	// A pull request's number is not an issue to comment on.
	_, pr := h.call(ordinary, sup.Token, "comment_issue", map[string]any{"project_id": "p1", "issue_number": 42, "body": "x", "request_id": "c2"})
	if !pr.Result.IsError || !strings.HasPrefix(pr.text(), "[not_found]") {
		t.Errorf("comment on a pull request = %q", pr.text())
	}
}

func TestIssueWritesFollowTheGrantAndProject(t *testing.T) {
	h := newIssuesHarness(t)
	main := h.seedAs(t, dbstate.RoleMain, dbstate.GrantCoordination, "")
	ws := h.seedAgent(t)
	sup := h.seedAs(t, dbstate.RoleSupervisor, dbstate.GrantCoordination, main.SessionID)
	if _, err := h.pool.Exec(context.Background(), `INSERT INTO projects (id, user_id, owner, name, full_name, html_url, default_branch)
		VALUES ('p2', 'owner', 'o', 'm', 'o/m', 'https://x', 'main'), ('foreign', 'someone', 'o', 'f', 'o/f', 'https://x', 'main')`); err != nil {
		t.Fatal(err)
	}
	h.fake.AddRepo(githubtest.Repository{Owner: "o", Name: "m", ID: 8})
	args := func(project, req string) map[string]any {
		return map[string]any{"project_id": project, "title": "T", "body": "B", "request_id": req}
	}

	// A code task does not hold the writes.
	if _, r := h.call(ordinary, ws.Token, "create_issue", args("p1", "w1")); r.Error == nil && !r.Result.IsError {
		t.Errorf("workspace agent create_issue = %q", r.text())
	}
	if n := h.fake.Count("POST", "/repos/o/n/issues"); n != 0 {
		t.Errorf("posts = %d", n)
	}
	// A coordination task writes to its own project only.
	if _, r := h.call(ordinary, sup.Token, "create_issue", args("p2", "s1")); !r.Result.IsError || !strings.HasPrefix(r.text(), "[forbidden]") {
		t.Errorf("supervisor in another project = %q", r.text())
	}
	// The main thread may name any of its owner's projects, and no one else's.
	if _, r := h.call(ordinary, main.Token, "create_issue", args("p2", "m1")); r.Result.IsError || r.Result.StructuredContent["repository"] != "o/m" {
		t.Errorf("main in p2 = %q", r.text())
	}
	if _, r := h.call(ordinary, main.Token, "create_issue", args("foreign", "m2")); !r.Result.IsError || !strings.HasPrefix(r.text(), "[not_found]") {
		t.Errorf("main in a foreign project = %q", r.text())
	}
	if _, r := h.call(ordinary, main.Token, "get_issue", map[string]any{"project_id": "p1", "issue_number": 5}); r.Result.IsError {
		t.Errorf("main get_issue = %q", r.text())
	}
}

func TestGetPullRequestRendersTheDiffFeedbackAndChecks(t *testing.T) {
	// Given a workspace agent bound to p1, whose pull request 44 changes one file, has a member's
	// review and one passing and one failing check
	h := newIssuesHarness(t)
	a := h.seedAgent(t)

	// When it reads the pull request
	_, r := h.call(ordinary, a.Token, "get_pull_request", map[string]any{"project_id": "p1", "pr_number": 44})

	// Then the text carries the description, the patch, the review and the failing check, and the
	// structured answer the same fields
	text := r.text()
	for _, want := range []string{"PR #44", "Adds x.", "--- x.go", "+package x", "[review APPROVED] bob", "LGTM", "## Checks: failed", "- test: failed"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	sc := r.Result.StructuredContent
	if r.Result.IsError || sc["number"] != float64(44) || sc["head_sha"] != checkRunsHeadSHA {
		t.Fatalf("get_pull_request = %v", sc)
	}
	files, _ := sc["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["patch_state"] != "full" {
		t.Errorf("files = %v", sc["files"])
	}

	// And another project, or a missing pull request, is refused
	_, other := h.call(ordinary, a.Token, "get_pull_request", map[string]any{"project_id": "p2", "pr_number": 44})
	if !other.Result.IsError || !strings.HasPrefix(other.text(), "[forbidden]") {
		t.Errorf("other project = %q", other.text())
	}
	_, miss := h.call(ordinary, a.Token, "get_pull_request", map[string]any{"project_id": "p1", "pr_number": 999})
	if !miss.Result.IsError || miss.text() != "[not_found] no such pull request" {
		t.Errorf("missing pull request = %q", miss.text())
	}
}
