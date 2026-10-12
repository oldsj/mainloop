package smoke

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
)

// MaxBudget is the ceiling on full live runs in one invocation: plan §7 item 1, the owner's approved
// quota for live-agent tests.
const MaxBudget = 3

// reconcileDeadline bounds the read-only reads that run after a failure.
const reconcileDeadline = 45 * time.Second

var (
	repoPattern    = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)
	projectPattern = regexp.MustCompile(`^[\w-]+$`)
)

// LiveOptions configures the live run. Nothing defaults: every target is named by the operator, so a
// run cannot start against a place nobody chose.
type LiveOptions struct {
	Endpoints           // API and GitPush are required
	APIHost      string // Host header for owner API requests, for port-forwards to a non-dev instance
	ProjectID    string
	Repo         string // owner/name of the disposable test repository
	AppLogin     string // the GitHub App login that must perform the merge
	Provider     string // claude or codex
	MergeTools   bool   // false skips the merge and completion steps
	Budget       int    // ceiling on full runs, from 1 to MaxBudget; every run ends the budget
	Deadline     time.Duration
	StepDeadline time.Duration
	PollInterval time.Duration
	Timeout      time.Duration // per request
	GitHub       GitHub
}

// Validate refuses a live run whose options are incomplete or out of range, before any request is sent.
func (o LiveOptions) Validate() error {
	switch {
	case o.Budget < 1 || o.Budget > MaxBudget:
		return fmt.Errorf("--budget must be from 1 to %d full runs", MaxBudget)
	case o.API == "" || o.GitPush == "":
		return errors.New("--base-url and --git-push-url are required with --live")
	case !projectPattern.MatchString(o.ProjectID):
		return errors.New("--project-id must be letters, digits, '-' or '_'")
	case !repoPattern.MatchString(o.Repo):
		return errors.New("--repo must be owner/name")
	case o.AppLogin == "":
		return errors.New("--app-login is required with --live")
	case o.Provider != "claude" && o.Provider != "codex":
		return errors.New("--provider must be claude or codex")
	case o.Deadline <= 0 || o.StepDeadline <= 0 || o.PollInterval <= 0 || o.Timeout <= 0:
		return errors.New("--deadline, --step-deadline, --poll-interval and --timeout must be positive")
	case o.GitHub == nil:
		return errors.New("a GitHub reader is required")
	}
	return nil
}

// Live runs one full run. --budget is a ceiling on full runs, but every outcome ends the budget: a run
// that completed has its proof, and a run that failed or skipped a step would spend quota to repeat the
// same gap. Each check reaches emit as it completes.
func Live(ctx context.Context, opts LiveOptions, emit func(Check)) {
	// The command validates first; this keeps a direct caller from sending anything with bad options.
	if err := opts.Validate(); err != nil {
		emit(failCheck("options", err))
		return
	}
	// gh is checked before any request, so a run without a GitHub login spends no quota.
	ready, cancel := context.WithTimeout(ctx, opts.Timeout)
	err := opts.GitHub.Ready(ready)
	cancel()
	if err != nil {
		emit(failCheck("github", err))
		return
	}
	c := newCaller(opts.APIHost, opts.Timeout)
	push := checkGitPush(ctx, c, opts.GitPush)
	emit(push)
	gitBlock := ""
	switch push.Status {
	case Skip:
		gitBlock = "git push transport off"
	case Fail:
		gitBlock = "git push probe failed"
	}
	mergeBlock := gitBlock
	if mergeBlock == "" && !opts.MergeTools {
		mergeBlock = "merge tools off"
	}
	run := &liveRun{opts: opts, c: c, emit: emit, n: 1, gitBlock: gitBlock, mergeBlock: mergeBlock}
	run.execute(ctx)
}

// liveRun is one full run: a chat submission, then the steps it must reach, in order.
type liveRun struct {
	opts       LiveOptions
	c          caller
	emit       func(Check)
	n          int
	gitBlock   string // non-empty when the git push transport cannot take a push
	mergeBlock string // non-empty when the merge step cannot run
	requestID  string
	branch     string
}

// report passes one check on to the caller.
func (r *liveRun) report(c Check) { r.emit(c) }

// runState is what the poll loop has read on its last pass.
type runState struct {
	view  *api.TaskView
	facts api.SmokeObservations
	pr    pull
}

// step is one milestone the run must reach. A step with skip set cannot run, and it is reported as
// skipped with that reason instead of being waited for.
type step struct {
	name   string
	skip   string
	done   func(*runState) bool
	detail func(*runState) string
}

// execute runs one full run. Its outcome always ends the budget; see Live.
func (r *liveRun) execute(ctx context.Context) {
	runCtx, cancel := context.WithTimeout(ctx, r.opts.Deadline)
	defer cancel()
	steps := r.steps()

	detail, err := r.preflight(runCtx)
	if err != nil {
		r.report(failCheck(r.name("preflight"), err))
		r.report(skipCheck(r.name("main-turn"), "not submitted: preflight failed"))
		r.skipRest(steps, 0, "not submitted: preflight failed")
		return
	}
	r.report(okCheck(r.name("preflight"), detail))

	r.requestID = newRequestID()
	r.branch = "smoke/" + r.opts.Provider + "-" + r.requestID

	// Exactly one submission. An accepted submission whose reply is lost is not retried: the
	// reconciliation reads after a failure find the task it created.
	var receipt api.ChatResponse
	if err := r.c.postJSON(runCtx, r.opts.API+"/chat", map[string]string{"message": r.prompt()}, &receipt); err != nil {
		r.report(failCheck(r.name("main-turn"), fmt.Errorf("%s: not retried: %w", r.identity(), err)))
		r.skipRest(steps, 0, "not reached: main turn failed")
		r.reconcile(ctx)
		return
	}
	r.report(okCheck(r.name("main-turn"), fmt.Sprintf("%s delivery_message_id=%s", r.identity(), deref(receipt.DeliveryMessageId))))

	if r.poll(runCtx, steps, &runState{}) {
		r.reconcile(ctx)
	}
}

// steps lists what a run must reach after its submission. The order is the order of the work.
func (r *liveRun) steps() []step {
	return []step{
		{
			name:   "delegation",
			done:   func(s *runState) bool { return s.view != nil },
			detail: func(s *runState) string { return "child task " + s.view.Task.Id },
		},
		{
			name:   "push",
			skip:   r.gitBlock,
			done:   func(s *runState) bool { return s.facts.PushConfirmed },
			detail: func(s *runState) string { return fmt.Sprintf("push confirmed, %d record(s)", len(s.facts.Pushes)) },
		},
		{
			name:   "pull-request",
			skip:   r.gitBlock,
			done:   func(s *runState) bool { return s.prNumber() != 0 },
			detail: func(s *runState) string { return fmt.Sprintf("pr #%d", s.prNumber()) },
		},
		{
			name:   "ci",
			skip:   r.gitBlock,
			done:   func(s *runState) bool { return s.projection().CiState == api.TaskProjectionCiStateSuccess },
			detail: func(s *runState) string { return "ci success on " + short(s.pr.Head.SHA) },
		},
		{
			name:   "merge",
			skip:   r.mergeBlock,
			done:   func(s *runState) bool { return s.pr.Merged },
			detail: func(s *runState) string { return "pr merged" },
		},
		{
			name: "completion",
			skip: r.mergeBlock,
			done: r.completed,
			detail: func(s *runState) string {
				return "merged by " + s.pr.MergedBy.Login + " at head " + short(s.pr.Head.SHA)
			},
		},
	}
}

// poll walks the steps in order, reading the task, the ledger and the pull request on each pass. It
// returns true when a step failed, after reporting that step and skipping the rest.
func (r *liveRun) poll(ctx context.Context, steps []step, st *runState) bool {
	idx, since := 0, time.Now()
	for idx < len(steps) {
		s := steps[idx]
		if s.skip != "" {
			r.report(skipCheck(r.name(s.name), s.skip))
			idx, since = idx+1, time.Now()
			continue
		}
		// The step's deadline bounds its reads too, so a GitHub read that never answers fails this step on time.
		stepCtx, cancel := context.WithDeadline(ctx, since.Add(r.opts.StepDeadline))
		err := r.refresh(stepCtx, st)
		cancel()
		if err != nil {
			return r.fail(ctx, steps, idx, readFailure(ctx, stepCtx, err))
		}
		// A step that is already done passes even if a later fact went bad, so a CI failure is charged to
		// the ci step, not to whichever step happened to be waiting when it appeared.
		if s.done(st) {
			r.report(okCheck(r.name(s.name), s.detail(st)))
			idx, since = idx+1, time.Now()
			continue
		}
		if err := st.terminal(); err != nil {
			return r.fail(ctx, steps, idx, err)
		}
		if ctx.Err() != nil {
			return r.fail(ctx, steps, idx, errors.New("overall deadline"))
		}
		if time.Since(since) >= r.opts.StepDeadline {
			return r.fail(ctx, steps, idx, errors.New("step deadline"))
		}
		if !sleep(ctx, min(r.opts.PollInterval, r.opts.StepDeadline-time.Since(since))) {
			return r.fail(ctx, steps, idx, errors.New("overall deadline"))
		}
	}
	return false
}

// fail reports the step that failed and skips the steps after it.
func (r *liveRun) fail(ctx context.Context, steps []step, idx int, err error) bool {
	if ctx.Err() != nil {
		err = errors.New("overall deadline")
	}
	r.report(failCheck(r.name(steps[idx].name), err))
	r.skipRest(steps, idx+1, "not reached: "+steps[idx].name+" failed")
	return true
}

// skipRest reports the steps from index from onward as skipped: with their own prerequisite reason
// when they have one, otherwise with fallback.
func (r *liveRun) skipRest(steps []step, from int, fallback string) {
	for _, s := range steps[from:] {
		reason := s.skip
		if reason == "" {
			reason = fallback
		}
		r.report(skipCheck(r.name(s.name), reason))
	}
}

// refresh reads the three facts a step can depend on: the child task, the ledger's record for this
// branch, and the pull request on GitHub once the task names one.
func (r *liveRun) refresh(ctx context.Context, st *runState) error {
	var views []api.TaskView
	if err := r.c.getJSON(ctx, r.opts.API+"/tasks?project_id="+url.QueryEscape(r.opts.ProjectID), &views); err != nil {
		return err
	}
	matches := r.matching(views)
	if len(matches) > 1 {
		return errors.New("multiple matching tasks")
	}
	st.view = nil
	if len(matches) == 1 {
		st.view = &matches[0]
	}
	var facts api.SmokeObservations
	if err := r.c.getJSON(ctx, r.observationsURL(r.branch), &facts); err != nil {
		return err
	}
	st.facts = facts
	if n := st.prNumber(); n != 0 {
		var p pull
		if err := r.readGitHub(ctx, fmt.Sprintf("pulls/%d", n), &p); err != nil {
			return err
		}
		st.pr = p
	}
	return nil
}

// reconcile reads what the run left behind, once, after a failure. It goes on past a failed read so
// one run shows every fault it can see, and it writes nothing.
func (r *liveRun) reconcile(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, reconcileDeadline)
	defer cancel()
	numbers := map[int]bool{}

	var views []api.TaskView
	if err := r.c.getJSON(ctx, r.opts.API+"/tasks?project_id="+url.QueryEscape(r.opts.ProjectID), &views); err != nil {
		r.report(failCheck(r.name("reconcile.tasks"), err))
	} else {
		rows := []taskRow{}
		for _, v := range r.matching(views) {
			row := taskRow{
				TaskID: v.Task.Id, Status: string(v.Task.Status), PR: v.Projection.PrNumber,
				CIState: string(v.Projection.CiState), MergeState: v.Projection.MergeState,
				PRHeadSHA: v.Projection.PrHeadSha, CIHeadSHA: v.Projection.CiHeadSha,
			}
			rows = append(rows, row)
			if v.Projection.PrNumber != nil {
				numbers[*v.Projection.PrNumber] = true
			}
		}
		r.report(okCheck(r.name("reconcile.tasks"), jsonDetail(rows)))
	}

	var facts api.SmokeObservations
	if err := r.c.getJSON(ctx, r.observationsURL(r.branch), &facts); err != nil {
		r.report(failCheck(r.name("reconcile.pushes"), err))
	} else {
		r.report(okCheck(r.name("reconcile.pushes"), jsonDetail(facts)))
	}

	var pulls []pull
	owner, _, _ := strings.Cut(r.opts.Repo, "/")
	if err := r.readGitHub(ctx, "pulls?state=all&head="+url.QueryEscape(owner+":"+r.branch), &pulls); err != nil {
		r.report(failCheck(r.name("reconcile.pulls"), err))
	} else {
		rows := []pullRow{}
		for _, p := range pulls {
			rows = append(rows, pullRow{Number: p.Number, State: p.State, MergedAt: p.MergedAt})
			numbers[p.Number] = true
		}
		r.report(okCheck(r.name("reconcile.pulls"), jsonDetail(rows)))
	}

	for _, n := range slices.Sorted(maps.Keys(numbers)) {
		r.reconcilePull(ctx, n)
	}
}

// reconcilePull reads one pull request and the check runs on its head commit.
func (r *liveRun) reconcilePull(ctx context.Context, n int) {
	var p pull
	if err := r.readGitHub(ctx, fmt.Sprintf("pulls/%d", n), &p); err != nil {
		r.report(failCheck(r.name(fmt.Sprintf("reconcile.pr-%d", n)), err))
		return
	}
	merged := map[string]any{"number": n, "merged": p.Merged, "merged_by": ""}
	if p.MergedBy != nil {
		merged["merged_by"] = p.MergedBy.Login
	}
	r.report(okCheck(r.name(fmt.Sprintf("reconcile.pr-%d", n)), jsonDetail(merged)))

	if p.Head.SHA == "" {
		r.report(failCheck(r.name(fmt.Sprintf("reconcile.ci-%d", n)), errors.New("pull request names no head commit")))
		return
	}
	var runs checkRuns
	if err := r.readGitHub(ctx, "commits/"+p.Head.SHA+"/check-runs", &runs); err != nil {
		r.report(failCheck(r.name(fmt.Sprintf("reconcile.ci-%d", n)), err))
		return
	}
	rows := []checkRow{}
	for _, cr := range runs.CheckRuns {
		rows = append(rows, checkRow{Name: cr.Name, Status: cr.Status, Conclusion: cr.Conclusion})
	}
	r.report(okCheck(r.name(fmt.Sprintf("reconcile.ci-%d", n)), jsonDetail(rows)))
}

// readGitHub reads one GitHub endpoint under the request timeout, so a read that never answers cannot hold
// up the step or the reconciliation that asked for it.
func (r *liveRun) readGitHub(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	return r.opts.GitHub.Get(ctx, r.opts.Repo, path, out)
}

// readFailure names why a read stopped short: the run's deadline or the step's, when either has passed.
// Otherwise it is the read's own error.
func readFailure(runCtx, stepCtx context.Context, err error) error {
	switch {
	case runCtx.Err() != nil:
		return errors.New("overall deadline")
	case stepCtx.Err() != nil:
		return errors.New("step deadline")
	}
	return err
}

// preflight refuses a run the project cannot take: a different repository, deliveries still in flight,
// or no free capacity. Nothing is submitted until it passes.
func (r *liveRun) preflight(ctx context.Context) (string, error) {
	var health api.HealthResponse
	if err := r.c.getJSON(ctx, r.opts.API+"/health", &health); err != nil {
		return "", err
	}
	if health.Status != api.Healthy {
		return "", fmt.Errorf("health status %s", health.Status)
	}
	var project api.Project
	if err := r.c.getJSON(ctx, r.opts.API+"/projects/"+url.PathEscape(r.opts.ProjectID), &project); err != nil {
		return "", err
	}
	if !strings.EqualFold(project.FullName, r.opts.Repo) {
		return "", errors.New("project repository differs from --repo")
	}
	var facts api.SmokeObservations
	if err := r.c.getJSON(ctx, r.observationsURL(""), &facts); err != nil {
		return "", err
	}
	switch {
	case facts.DeliveriesBusy:
		return "", errors.New("deliveries in flight from an earlier run")
	case len(facts.CapacityHolders) >= facts.ParentCapacity:
		return "", fmt.Errorf("parent capacity held by %s", strings.Join(facts.CapacityHolders, ","))
	case !facts.GlobalCapacityAvailable:
		return "", errors.New("global capacity unavailable")
	}
	return "project matches, no deliveries in flight, capacity free", nil
}

// completed binds the merge to this repository, this branch and this head: the task, the ledger and
// GitHub must all agree, and the merger must be the App login the operator named.
func (r *liveRun) completed(s *runState) bool {
	if s.view == nil {
		return false
	}
	p := s.view.Projection
	head := s.pr.Head
	return string(s.view.Task.Status) == "completed" &&
		deref(p.MergeState) == "merged" &&
		s.pr.Merged &&
		s.pr.MergedBy != nil && s.pr.MergedBy.Login == r.opts.AppLogin &&
		s.facts.PushConfirmed &&
		strings.EqualFold(s.pr.Base.repoName(), r.opts.Repo) &&
		strings.EqualFold(head.repoName(), r.opts.Repo) &&
		head.Ref == r.branch &&
		deref(p.PrHeadSha) != "" && head.SHA == deref(p.PrHeadSha) &&
		deref(p.CiHeadSha) == head.SHA
}

func (r *liveRun) matching(views []api.TaskView) []api.TaskView {
	var out []api.TaskView
	for _, v := range views {
		if v.Task.Checkout != nil && v.Task.Checkout.Branch == r.branch {
			out = append(out, v)
		}
	}
	return out
}

func (r *liveRun) observationsURL(branch string) string {
	u := r.opts.API + "/projects/" + url.PathEscape(r.opts.ProjectID) + "/smoke-observations"
	if branch != "" {
		u += "?branch=" + url.QueryEscape(branch)
	}
	return u
}

func (r *liveRun) prompt() string {
	return fmt.Sprintf("Delegate exactly one code task on project %s, repository %s, provider_profile_id %s, "+
		"checkout.branch %s, request_id %s. Create only docs/smoke-%s.md containing a short smoke verification note. "+
		"Commit the file, git push through the publication gate, open_pull_request, "+
		"wait for exact-head CI using foreground waits, merge_pull_request, then report completed. "+
		"Never bypass policy or answer approval cards. Do not delegate additional tasks.",
		r.opts.ProjectID, r.opts.Repo, r.opts.Provider, r.branch, r.requestID, r.requestID)
}

func (r *liveRun) name(step string) string { return fmt.Sprintf("run%d.%s", r.n, step) }

func (r *liveRun) identity() string {
	return fmt.Sprintf("request_id=%s branch=%s", r.requestID, r.branch)
}

func (s *runState) projection() api.TaskProjection {
	if s.view == nil {
		return api.TaskProjection{}
	}
	return s.view.Projection
}

func (s *runState) prNumber() int {
	return deref(s.projection().PrNumber)
}

// terminal reports a state that can no longer succeed, so the run need not wait out its deadline.
func (s *runState) terminal() error {
	if s.view == nil {
		return nil
	}
	switch s.view.Task.Status {
	case api.TaskStatusFailed, api.TaskStatusCancelled, api.TaskStatusBlocked:
		return fmt.Errorf("task %s", s.view.Task.Status)
	}
	if s.projection().CiState == api.TaskProjectionCiStateFailure {
		return errors.New("ci failed")
	}
	return nil
}

// GitHub reads the test repository. Paths are relative to repos/{repo}/.
type GitHub interface {
	Get(ctx context.Context, repo, path string, out any) error
	// Ready reports whether the reader can answer at all, so a live run refuses before it spends quota.
	Ready(ctx context.Context) error
}

// GHCLI reads GitHub with the operator's gh login. It runs gh with argv only, never through a shell, and
// reports a failure without gh's output, which can echo request details.
type GHCLI struct{}

// Get reads one endpoint with gh api and decodes its JSON.
func (GHCLI) Get(ctx context.Context, repo, path string, out any) error {
	cmd := exec.CommandContext(ctx, "gh", "api", "repos/"+repo+"/"+path) //nolint:gosec // argv only; repo was validated and path is built here
	raw, err := cmd.Output()
	if err != nil {
		return errors.New("GitHub read failed")
	}
	return json.Unmarshal(raw, out)
}

// Ready checks that gh is installed and has a login. Its output is discarded, as it is for Get.
func (GHCLI) Ready(ctx context.Context) error {
	if _, err := exec.LookPath("gh"); err != nil {
		return errors.New("gh is not installed: --live reads GitHub through the operator's gh login")
	}
	err := exec.CommandContext(ctx, "gh", "auth", "status").Run()
	if ctx.Err() != nil {
		return errors.New("gh auth status did not answer in time")
	}
	if err != nil {
		return errors.New("gh is not logged in: run gh auth login, then retry")
	}
	return nil
}

// pull is the subset of a GitHub pull request the run reads.
type pull struct {
	Number   int      `json:"number"`
	State    string   `json:"state"`
	Merged   bool     `json:"merged"`
	MergedAt *string  `json:"merged_at"`
	MergedBy *account `json:"merged_by"`
	Base     pullSide `json:"base"`
	Head     pullSide `json:"head"`
}

type account struct {
	Login string `json:"login"`
}

type pullSide struct {
	Ref  string   `json:"ref"`
	SHA  string   `json:"sha"`
	Repo *repoRef `json:"repo"`
}

type repoRef struct {
	FullName string `json:"full_name"`
}

func (p pullSide) repoName() string {
	if p.Repo == nil {
		return ""
	}
	return p.Repo.FullName
}

type checkRuns struct {
	CheckRuns []struct {
		Name       string  `json:"name"`
		Status     string  `json:"status"`
		Conclusion *string `json:"conclusion"`
	} `json:"check_runs"`
}

type taskRow struct {
	TaskID     string  `json:"task_id"`
	Status     string  `json:"status"`
	PR         *int    `json:"pr_number"`
	CIState    string  `json:"ci_state"`
	MergeState *string `json:"merge_state"`
	PRHeadSHA  *string `json:"pr_head_sha"`
	CIHeadSHA  *string `json:"ci_head_sha"`
}

type pullRow struct {
	Number   int     `json:"number"`
	State    string  `json:"state"`
	MergedAt *string `json:"merged_at"`
}

type checkRow struct {
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
}

// sleep waits for d or until ctx ends. It reports false when ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// newRequestID returns 32 random hex digits, the same shape as the Python script's uuid4 hex.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand does not fail on the platforms this runs on
	return hex.EncodeToString(b[:])
}

func jsonDetail(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "unencodable: " + err.Error()
	}
	return string(raw)
}

func deref[T any](p *T) (v T) {
	if p != nil {
		v = *p
	}
	return v
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
