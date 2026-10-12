package smoke

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
)

const (
	testRepo     = "oldsj/smoke-repo"
	testAppLogin = "mainloop-app"
	testProject  = "p1"
)

// branchPattern reads the branch the prompt asks the agent to use: "checkout.branch smoke/..., request_id".
var branchPattern = regexp.MustCompile(`checkout\.branch (\S+),`)

// fakeLive plays the owner API and GitHub for one live run. Each read of /tasks advances the stage by one,
// so the run's steps complete in order: task at stage 1, push at 2, pull request at 2, CI at 3, merge at 4.
type fakeLive struct {
	mu        sync.Mutex
	branch    string
	posts     int
	reads     int  // reads of /tasks, which is the stage
	ciFail    bool // CI fails from stage 3 on
	busy      bool // deliveries_busy: preflight must refuse
	chatCode  int  // non-zero makes POST /chat answer with this status
	neverPush bool // push_confirmed stays false, so the push step has to time out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// serve starts the owner API routes a live run reads.
func (f *fakeLive) serve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", jsonHandler(`{"status":"healthy","database":"ok"}`))
	mux.HandleFunc("/projects/"+testProject, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"full_name": testRepo})
	})
	mux.HandleFunc("/projects/"+testProject+"/smoke-observations", f.observations)
	mux.HandleFunc("/chat", f.chat)
	mux.HandleFunc("/tasks", f.tasks)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *fakeLive) observations(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON(w, map[string]any{
		"capacity_holders":          []string{},
		"deliveries":                []any{},
		"deliveries_busy":           f.busy,
		"global_capacity_available": true,
		"parent_capacity":           2,
		"push_confirmed":            f.reads >= 2 && !f.neverPush,
		"pushes":                    []any{},
	})
}

func (f *fakeLive) chat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts++
	if m := branchPattern.FindStringSubmatch(in.Message); m != nil {
		f.branch = m[1]
	}
	if f.chatCode != 0 {
		w.WriteHeader(f.chatCode)
		return
	}
	writeJSON(w, map[string]any{"conversation_id": "c1", "delivery_message_id": "d1", "pending": true})
}

func (f *fakeLive) tasks(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.branch == "" {
		writeJSON(w, []any{})
		return
	}
	status, ci := "running", "unknown"
	var prNumber, prSHA, ciSHA, merge any
	if f.reads >= 2 {
		prNumber, prSHA, ci = 7, "abc123", "pending"
	}
	if f.reads >= 3 {
		ci, ciSHA = "success", "abc123"
		if f.ciFail {
			ci = "failure"
		}
	}
	if f.reads >= 4 {
		status, merge = "completed", "merged"
	}
	writeJSON(w, []map[string]any{{
		"task": map[string]any{
			"id": "t1", "status": status,
			"checkout": map[string]string{"branch": f.branch},
		},
		"projection": map[string]any{
			"ci_state": ci, "ci_head_sha": ciSHA, "merge_state": merge,
			"pr_number": prNumber, "pr_head_sha": prSHA,
		},
	}})
}

// answer is the GitHub side: the pull request on head abc123, its check runs, and the pull list.
func (f *fakeLive) answer(path string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	merged := f.reads >= 4
	switch {
	case strings.HasPrefix(path, "pulls?"):
		return []map[string]any{{"number": 7, "state": "open", "merged_at": nil}}, nil
	case path == "pulls/7":
		var mergedBy, mergedAt any
		if merged {
			mergedBy = map[string]string{"login": testAppLogin}
			mergedAt = "2026-10-11T00:00:00Z"
		}
		side := func(ref string) map[string]any {
			return map[string]any{"ref": ref, "sha": "abc123", "repo": map[string]string{"full_name": testRepo}}
		}
		return map[string]any{
			"number": 7, "state": "open", "merged": merged, "merged_at": mergedAt, "merged_by": mergedBy,
			"base": side("main"), "head": side(f.branch),
		}, nil
	case path == "commits/abc123/check-runs":
		conclusion := "success"
		if f.ciFail {
			conclusion = "failure"
		}
		return map[string]any{"check_runs": []map[string]any{{"name": "ci", "status": "completed", "conclusion": conclusion}}}, nil
	}
	return nil, errors.New("unexpected GitHub path " + path)
}

// fakeGitHub reads GitHub answers through the same JSON round trip the real client does.
type fakeGitHub func(path string) (any, error)

func (g fakeGitHub) Get(_ context.Context, _, path string, out any) error {
	v, err := g(path)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// Ready answers as a logged-in gh would.
func (fakeGitHub) Ready(context.Context) error { return nil }

// blockingGitHub never answers: each read waits until its context ends, as a hung GitHub does.
type blockingGitHub struct{}

func (blockingGitHub) Get(ctx context.Context, _, _ string, _ any) error {
	<-ctx.Done()
	return ctx.Err()
}

func (blockingGitHub) Ready(context.Context) error { return nil }

// readyFailGitHub is a reader whose gh is not logged in.
type readyFailGitHub struct{ blockingGitHub }

func (readyFailGitHub) Ready(context.Context) error { return errors.New("gh is not logged in") }

// liveOptions returns options for one run against the fake, with the git transport on and merge tools on.
func liveOptions(t *testing.T, f *fakeLive, budget int) LiveOptions {
	t.Helper()
	return LiveOptions{
		Endpoints:    Endpoints{API: f.serve(t), GitPush: gitStub(t, "git-receive-pack", http.StatusUnauthorized, "authentication_required req-1")},
		ProjectID:    testProject,
		Repo:         testRepo,
		AppLogin:     testAppLogin,
		Provider:     "claude",
		MergeTools:   true,
		Budget:       budget,
		Deadline:     10 * time.Second,
		StepDeadline: 5 * time.Second,
		PollInterval: time.Millisecond,
		Timeout:      5 * time.Second,
		GitHub:       fakeGitHub(f.answer),
	}
}

func runLive(opts LiveOptions) []Check {
	var checks []Check
	Live(context.Background(), opts, func(c Check) { checks = append(checks, c) })
	return checks
}

// statusesOf maps each check name to its status, for asserting a run's whole shape at once.
func statusesOf(checks []Check) map[string]Check {
	out := map[string]Check{}
	for _, c := range checks {
		out[c.Name] = c
	}
	return out
}

func TestLiveHappyPathReachesCompletion(t *testing.T) {
	f := &fakeLive{}
	checks := runLive(liveOptions(t, f, 1))
	got := statusesOf(checks)
	for _, name := range []string{
		"git-push", "run1.preflight", "run1.main-turn", "run1.delegation", "run1.push",
		"run1.pull-request", "run1.ci", "run1.merge", "run1.completion",
	} {
		if c, ok := got[name]; !ok || c.Status != OK {
			t.Errorf("%s = %v, want ok", name, c)
		}
	}
	if f.posts != 1 {
		t.Errorf("chat posts = %d, want exactly 1", f.posts)
	}
	if !CompletionProven(checks) {
		t.Errorf("completion not proven: %v", checks)
	}
}

func TestLiveCIFailureReconcilesAndStopsBudget(t *testing.T) {
	f := &fakeLive{ciFail: true}
	checks := runLive(liveOptions(t, f, 2))
	got := statusesOf(checks)
	if c := got["run1.ci"]; c.Status != Fail || c.Detail != "ci failed" {
		t.Errorf("run1.ci = %v, want fail ci failed", c)
	}
	if c := got["run1.merge"]; c.Status != Skip || c.Detail != "not reached: ci failed" {
		t.Errorf("run1.merge = %v, want skip not reached", c)
	}
	for _, name := range []string{"run1.reconcile.tasks", "run1.reconcile.pulls", "run1.reconcile.pr-7", "run1.reconcile.ci-7"} {
		if c, ok := got[name]; !ok || c.Status != OK {
			t.Errorf("%s = %v, want ok reconciliation read", name, c)
		}
	}
	for _, c := range checks {
		if strings.HasPrefix(c.Name, "run2.") {
			t.Errorf("a second run started after a failure: %s", c)
		}
	}
	if f.posts != 1 {
		t.Errorf("chat posts = %d, want 1: a failed run must not spend another turn", f.posts)
	}
	if CompletionProven(checks) {
		t.Error("completion reported proven for a failed run")
	}
}

func TestLivePreflightRefusalSubmitsNothing(t *testing.T) {
	f := &fakeLive{busy: true}
	checks := runLive(liveOptions(t, f, 2))
	got := statusesOf(checks)
	if c := got["run1.preflight"]; c.Status != Fail || !strings.Contains(c.Detail, "in flight") {
		t.Errorf("run1.preflight = %v, want fail on deliveries in flight", c)
	}
	if c := got["run1.main-turn"]; c.Status != Skip || c.Detail != "not submitted: preflight failed" {
		t.Errorf("run1.main-turn = %v, want skip not submitted", c)
	}
	if f.posts != 0 {
		t.Errorf("chat posts = %d after a refused preflight, want 0", f.posts)
	}
}

// A direct caller with a zero budget gets one failed options check and no request, even though the
// command layer would have refused it first.
func TestLiveRefusesInvalidOptionsBeforeAnyRequest(t *testing.T) {
	f := &fakeLive{}
	checks := runLive(liveOptions(t, f, 0))
	if len(checks) != 1 || checks[0].Name != "options" || checks[0].Status != Fail {
		t.Fatalf("checks = %v, want one failed options check", checks)
	}
	if f.posts != 0 {
		t.Errorf("chat posts = %d after invalid options, want 0", f.posts)
	}
}

func TestLiveSubmissionFailureIsNotRetried(t *testing.T) {
	f := &fakeLive{chatCode: http.StatusInternalServerError}
	checks := runLive(liveOptions(t, f, 2))
	got := statusesOf(checks)
	if c := got["run1.main-turn"]; c.Status != Fail || !strings.Contains(c.Detail, "not retried") {
		t.Errorf("run1.main-turn = %v, want fail marked not retried", c)
	}
	if c := got["run1.delegation"]; c.Status != Skip || c.Detail != "not reached: main turn failed" {
		t.Errorf("run1.delegation = %v, want skip not reached", c)
	}
	if f.posts != 1 {
		t.Errorf("chat posts = %d, want exactly 1", f.posts)
	}
}

func TestLiveSkipsPublicationWhenTransportOff(t *testing.T) {
	f := &fakeLive{}
	opts := liveOptions(t, f, 2)
	opts.GitPush = gitStub(t, "git-receive-pack", http.StatusForbidden, "git_transport_disabled req-1")
	checks := runLive(opts)
	got := statusesOf(checks)
	if c := got["git-push"]; c.Status != Skip {
		t.Errorf("git-push = %v, want skip", c)
	}
	if c := got["run1.delegation"]; c.Status != OK {
		t.Errorf("run1.delegation = %v, want ok: the task is still submitted", c)
	}
	for _, name := range []string{"run1.push", "run1.pull-request", "run1.ci", "run1.merge", "run1.completion"} {
		if c := got[name]; c.Status != Skip || c.Detail != "git push transport off" {
			t.Errorf("%s = %v, want skip git push transport off", name, c)
		}
	}
	if f.posts != 1 || CompletionProven(checks) {
		t.Errorf("posts = %d, proven = %v; want one submission and no proof", f.posts, CompletionProven(checks))
	}
}

func TestLiveSkipsMergeWhenMergeToolsOff(t *testing.T) {
	f := &fakeLive{}
	opts := liveOptions(t, f, 1)
	opts.MergeTools = false
	checks := runLive(opts)
	got := statusesOf(checks)
	if c := got["run1.ci"]; c.Status != OK {
		t.Errorf("run1.ci = %v, want ok: CI does not depend on merge tools", c)
	}
	for _, name := range []string{"run1.merge", "run1.completion"} {
		if c := got[name]; c.Status != Skip || c.Detail != "merge tools off" {
			t.Errorf("%s = %v, want skip merge tools off", name, c)
		}
	}
	if CompletionProven(checks) {
		t.Error("completion proven with merge tools off")
	}
}

func TestLiveValidateRefusesBadOptions(t *testing.T) {
	good := LiveOptions{
		Endpoints: Endpoints{API: "http://api", GitPush: "http://push"},
		ProjectID: testProject, Repo: testRepo, AppLogin: testAppLogin, Provider: "codex",
		Budget: 1, Deadline: time.Minute, StepDeadline: time.Minute, PollInterval: time.Second, Timeout: time.Second,
		GitHub: fakeGitHub(func(string) (any, error) { return nil, nil }),
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid options refused: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(o *LiveOptions)
		want   string
	}{
		{"no budget", func(o *LiveOptions) { o.Budget = 0 }, "--budget"},
		{"budget above the owner cap", func(o *LiveOptions) { o.Budget = MaxBudget + 1 }, "--budget"},
		{"no API", func(o *LiveOptions) { o.API = "" }, "--base-url"},
		{"no push listener", func(o *LiveOptions) { o.GitPush = "" }, "--git-push-url"},
		{"project with a slash", func(o *LiveOptions) { o.ProjectID = "p/1" }, "--project-id"},
		{"repo without an owner", func(o *LiveOptions) { o.Repo = "smoke-repo" }, "--repo"},
		{"no app login", func(o *LiveOptions) { o.AppLogin = "" }, "--app-login"},
		{"unknown provider", func(o *LiveOptions) { o.Provider = "gemini" }, "--provider"},
		{"zero deadline", func(o *LiveOptions) { o.Deadline = 0 }, "positive"},
		{"no GitHub reader", func(o *LiveOptions) { o.GitHub = nil }, "GitHub reader"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := good
			tc.mutate(&o)
			err := o.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestCompletedBindsTheMergeToOneHead(t *testing.T) {
	opts := LiveOptions{Repo: testRepo, AppLogin: testAppLogin}
	r := &liveRun{opts: opts, branch: "smoke/claude-x"}
	base := func() *runState {
		return &runState{
			view: &api.TaskView{
				Task:       api.Task{Status: api.TaskStatusCompleted, Checkout: &api.TaskCheckoutOutput{Branch: "smoke/claude-x"}},
				Projection: api.TaskProjection{MergeState: strPtr("merged"), PrHeadSha: strPtr("abc"), CiHeadSha: strPtr("abc")},
			},
			facts: api.SmokeObservations{PushConfirmed: true},
			pr: pull{
				Merged:   true,
				MergedBy: &account{Login: testAppLogin},
				Base:     pullSide{Ref: "main", Repo: &repoRef{FullName: testRepo}},
				Head:     pullSide{Ref: "smoke/claude-x", SHA: "abc", Repo: &repoRef{FullName: testRepo}},
			},
		}
	}
	if !r.completed(base()) {
		t.Fatal("completed() refused a fully consistent run")
	}
	tests := []struct {
		name   string
		mutate func(s *runState)
	}{
		{"no task", func(s *runState) { s.view = nil }},
		{"task still running", func(s *runState) { s.view.Task.Status = api.TaskStatusRunning }},
		{"ledger not merged", func(s *runState) { s.view.Projection.MergeState = strPtr("open") }},
		{"pull request not merged", func(s *runState) { s.pr.Merged = false }},
		{"merged by another login", func(s *runState) { s.pr.MergedBy = &account{Login: "someone"} }},
		{"push not confirmed", func(s *runState) { s.facts.PushConfirmed = false }},
		{"base in another repo", func(s *runState) { s.pr.Base.Repo.FullName = "fork/smoke-repo" }},
		{"head from a fork", func(s *runState) { s.pr.Head.Repo.FullName = "fork/smoke-repo" }},
		{"head on another branch", func(s *runState) { s.pr.Head.Ref = "main" }},
		{"no PR head recorded", func(s *runState) { s.view.Projection.PrHeadSha = strPtr("") }},
		{"merged head differs from the recorded head", func(s *runState) { s.pr.Head.SHA = "def" }},
		{"CI ran on another commit", func(s *runState) { s.view.Projection.CiHeadSha = strPtr("def") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := base()
			tc.mutate(s)
			if r.completed(s) {
				t.Error("completed() accepted an inconsistent run")
			}
		})
	}
}

func strPtr(s string) *string { return &s }

// --budget is a ceiling, and a run's outcome ends it: a budget of three still posts once after a pass.
func TestLiveBudgetOfThreeStillPostsOnce(t *testing.T) {
	f := &fakeLive{}
	checks := runLive(liveOptions(t, f, 3))
	if !CompletionProven(checks) {
		t.Errorf("completion not proven: %v", checks)
	}
	if f.posts != 1 {
		t.Errorf("chat posts = %d, want 1: a completed run must not be repeated", f.posts)
	}
	for _, c := range checks {
		if strings.HasPrefix(c.Name, "run2.") {
			t.Errorf("a second run started after a completed run: %s", c)
		}
	}
}

func TestLiveStepDeadlineFailsAStepThatNeverProgresses(t *testing.T) {
	f := &fakeLive{neverPush: true}
	opts := liveOptions(t, f, 1)
	opts.StepDeadline = 200 * time.Millisecond
	start := time.Now()
	got := statusesOf(runLive(opts))
	if c := got["run1.push"]; c.Status != Fail || c.Detail != "step deadline" {
		t.Errorf("run1.push = %v, want fail step deadline", c)
	}
	if c := got["run1.pull-request"]; c.Status != Skip || c.Detail != "not reached: push failed" {
		t.Errorf("run1.pull-request = %v, want skip not reached", c)
	}
	if elapsed := time.Since(start); elapsed >= opts.Deadline {
		t.Errorf("run took %s, the whole run deadline: the step deadline did not end the step", elapsed)
	}
}

func TestLiveRunDeadlineEndsAStalledRun(t *testing.T) {
	f := &fakeLive{neverPush: true}
	opts := liveOptions(t, f, 1)
	opts.Deadline = 150 * time.Millisecond
	opts.StepDeadline = 10 * time.Second
	got := statusesOf(runLive(opts))
	if c := got["run1.push"]; c.Status != Fail || c.Detail != "overall deadline" {
		t.Errorf("run1.push = %v, want fail overall deadline", c)
	}
}

// A GitHub read that never answers fails its step at the step deadline, and the reconciliation after it
// stops at the request timeout. Without either bound the run would wait out its whole deadline.
func TestLiveGitHubReadStopsAtTheStepDeadline(t *testing.T) {
	f := &fakeLive{}
	opts := liveOptions(t, f, 1)
	opts.GitHub = blockingGitHub{}
	opts.StepDeadline = 200 * time.Millisecond
	opts.Timeout = 300 * time.Millisecond
	start := time.Now()
	got := statusesOf(runLive(opts))
	if c := got["run1.push"]; c.Status != Fail || c.Detail != "step deadline" {
		t.Errorf("run1.push = %v, want fail step deadline", c)
	}
	if elapsed := time.Since(start); elapsed >= opts.Deadline {
		t.Errorf("run took %s, the whole run deadline: a hung GitHub read held the run", elapsed)
	}
}

// readGitHub gives up at the request timeout even when the caller has no deadline of its own.
func TestReadGitHubIsBoundedByTheRequestTimeout(t *testing.T) {
	r := &liveRun{opts: LiveOptions{Repo: testRepo, Timeout: 50 * time.Millisecond, GitHub: blockingGitHub{}}}
	var p pull
	if err := r.readGitHub(context.Background(), "pulls/7", &p); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("readGitHub() = %v, want the request timeout", err)
	}
}

// A reader whose gh is not logged in refuses the run before its first request.
func TestLiveStopsBeforeAnyRunWhenGitHubIsNotReady(t *testing.T) {
	f := &fakeLive{}
	opts := liveOptions(t, f, 1)
	opts.GitHub = readyFailGitHub{}
	checks := runLive(opts)
	if len(checks) != 1 || checks[0].Name != "github" || checks[0].Status != Fail {
		t.Fatalf("checks = %v, want one failed github check", checks)
	}
	if !strings.Contains(checks[0].Detail, "not logged in") {
		t.Errorf("github detail = %q, want the login message", checks[0].Detail)
	}
	if f.posts != 0 {
		t.Errorf("chat posts = %d, want 0", f.posts)
	}
}

// With no gh on PATH the reader names the install problem.
func TestGHCLIReadyNamesAMissingGH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := (GHCLI{}).Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("Ready() = %v, want a not-installed error", err)
	}
}
