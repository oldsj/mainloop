package merge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oldsj/mainloop/server/internal/github"
)

// GitHub is the part of the GitHub App that merging uses.
type GitHub interface {
	MergeSettings(ctx context.Context, repo github.Repo) (*github.MergeRepo, error)
	BranchTip(ctx context.Context, repo github.Repo, branch string) (string, error)
	PullRequest(ctx context.Context, repo github.Repo, number int) (*github.PullRequestInfo, error)
	PullFiles(ctx context.Context, repo github.Repo, number, page, perPage int) ([]github.PullFile, bool, error)
	Compare(ctx context.Context, repo github.Repo, base, head string, perPage, page int) (*github.Comparison, error)
	MergePullRequest(ctx context.Context, repo github.Repo, number int, sha string) (*github.MergeResult, error)
	CheckSuites(ctx context.Context, repo github.Repo, sha string) ([]github.CheckSuite, error)
	CheckRuns(ctx context.Context, repo github.Repo, sha string) ([]github.CheckRun, error)
	CommitStatuses(ctx context.Context, repo github.Repo, sha string) ([]github.CommitStatus, error)
	BranchProtection(ctx context.Context, repo github.Repo, branch string) (*github.Protection, error)
	BranchRules(ctx context.Context, repo github.Repo, branch string) ([]github.BranchRule, error)
}

// Reasons evidence is unavailable. They are stable, and are shown to the agent and the owner.
const (
	ReasonNotOpen         = "pull_request_not_open"
	ReasonDraft           = "pull_request_is_draft"
	ReasonRepository      = "repository_mismatch"
	ReasonBase            = "base_is_not_default_branch"
	ReasonHeadIsDefault   = "head_is_default_branch"
	ReasonSquash          = "squash_merge_not_allowed"
	ReasonArchived        = "repository_archived"
	ReasonTooManyFiles    = "too_many_files"
	ReasonFilesIncomplete = "changed_files_incomplete"
	ReasonInvalidPath     = "invalid_changed_path"
	ReasonMergeQueue      = "merge_queue_not_supported"
	ReasonHeadRepo        = "head_repository_unavailable"
)

// Check results.
const (
	resultPassed  = "passed"
	resultFailed  = "failed"
	resultPending = "pending"
)

// abandonedSuiteGrace is how long a queued suite with no runs may sit before it stops counting.
const abandonedSuiteGrace = 10 * time.Minute

// errStale means GitHub changed while the evidence was being read. The reading is discarded.
var errStale = errors.New("merge evidence changed while it was read")

// FileFact is one changed file as frozen in a proposal.
type FileFact struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename,omitempty"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
}

// CheckFact is one CI result as frozen in a proposal: a check run, a commit status, a suite
// that has not succeeded, or a required check that has no result (yet).
type CheckFact struct {
	Kind    string `json:"kind"` // check_run | commit_status | suite | required_check
	AppID   int64  `json:"app_id,omitempty"`
	Context string `json:"context,omitempty"`
	Status  string `json:"status"`
	Result  string `json:"result"`
}

// Facts is everything a proposal froze. A proposal's presentation (the contract's MergeSummary)
// and digest derive from it, and a later evaluation compares what it reads again to it.
type Facts struct {
	Version      int    `json:"version"`
	Repository   string `json:"repository"`
	RepositoryID int64  `json:"repository_id"`
	PRNumber     int    `json:"pr_number"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	Head         string `json:"head"`
	HeadSHA      string `json:"head_sha"`
	Base         string `json:"base"`
	// BaseSHA is the default branch's tip when the evidence was read, which is what a squash lands
	// on. The pull request's own recorded base only moves when the pull request is synchronised.
	BaseSHA      string     `json:"base_sha"`
	MergeBaseSHA string     `json:"merge_base_sha"`
	Files        []FileFact `json:"files"`
	// MergeFiles are the paths a squash onto BaseSHA changes, from the compare-based inventory.
	MergeFiles       []FileFact       `json:"merge_files"`
	MergeFilesDigest string           `json:"merge_files_digest"`
	Additions        int              `json:"additions"`
	Deletions        int              `json:"deletions"`
	PathsDigest      string           `json:"paths_digest"`
	Policy           string           `json:"policy"`
	PolicyVer        int              `json:"policy_version"`
	GlobsVersion     int              `json:"globs_version"`
	Protected        []ProtectedMatch `json:"protected"`
	Route            string           `json:"route"`
	Checks           []CheckFact      `json:"checks"`
	// IgnoredSuites counts queued suites that never produced runs and sat past the grace period.
	IgnoredSuites int `json:"ignored_suites"`
	// Required lists the required status checks of branch protection and rulesets.
	Required []RequiredCheck `json:"required"`
	// RulesUnavailable names the reads GitHub refused with its plan message, which it does for
	// repositories whose plan has no branch protection or rulesets: "protection" and/or "rules".
	RulesUnavailable []string  `json:"github_rules_unavailable_on_plan"`
	Unavailable      []string  `json:"unavailable"`
	CapturedAt       time.Time `json:"captured_at"`
}

// Evidence is what one reading of GitHub found.
type Evidence struct {
	Facts Facts
	PR    github.PullRequestInfo
}

// ciCounts summarises the checks.
type ciCounts struct{ Passed, Failed, Pending, Results int }

func countChecks(checks []CheckFact) ciCounts {
	var c ciCounts
	for _, ch := range checks {
		if ch.Kind == "required_check" {
			// A required check without a result counts as pending, but is not a result.
			c.Pending++
			continue
		}
		c.Results++
		switch ch.Result {
		case resultPassed:
			c.Passed++
		case resultFailed:
			c.Failed++
		default:
			c.Pending++
		}
	}
	return c
}

// green reports whether the checks allow a merge: nothing failed or pending, every required check
// passed, and at least one result exists, so a repository without CI never looks green.
func green(c ciCounts) bool { return c.Failed == 0 && c.Pending == 0 && c.Results > 0 }

// pendingStates are the in-flight states of a check run, suite or commit status.
var pendingStates = []string{"queued", "in_progress", "pending", "waiting", "requested"}

// checkResult judges a run or suite. Only a completed success passes: neutral, skipped and every
// unfamiliar conclusion or state fail, so a result GitHub reports in a way Mainloop does not know
// is never green.
func checkResult(status, conclusion string) string {
	switch {
	case status == "completed" && conclusion == "success":
		return resultPassed
	case status == "completed":
		return resultFailed
	case slices.Contains(pendingStates, status):
		return resultPending
	}
	return resultFailed
}

func statusResult(state string) string {
	switch state {
	case "success":
		return resultPassed
	case "pending":
		return resultPending
	}
	return resultFailed
}

// gather reads the pull request, its files and its CI facts. A GitHub failure is an error, to be
// retried or reported; a pull request that cannot be merged on principle is reported in
// Facts.Unavailable instead. Nothing it reads is trusted if GitHub changed meanwhile: the pull
// request, the repository and the default branch are read again at the end and must be the same.
func gather(ctx context.Context, gh GitHub, repo github.Repo, number int, now time.Time) (*Evidence, error) {
	pr, err := gh.PullRequest(ctx, repo, number)
	if err != nil {
		return nil, fmt.Errorf("read pull request: %w", err)
	}
	detail, err := gh.MergeSettings(ctx, repo)
	if err != nil {
		return nil, readErr("read repository", err)
	}
	f := Facts{
		Version: 1, Repository: detail.FullName, RepositoryID: detail.ID, PRNumber: pr.Number,
		Title: pr.Title, Body: pr.Body, Head: pr.Head.Ref, HeadSHA: pr.Head.SHA, Base: pr.Base.Ref,
		Additions: pr.Additions, Deletions: pr.Deletions,
		GlobsVersion: GlobsVersion, CapturedAt: now.UTC().Truncate(time.Second),
		Files: []FileFact{}, MergeFiles: []FileFact{}, Protected: []ProtectedMatch{}, Checks: []CheckFact{},
		Required: []RequiredCheck{}, RulesUnavailable: []string{}, Unavailable: []string{},
	}
	bad := func(reason string) { f.Unavailable = append(f.Unavailable, reason) }

	switch {
	case pr.Number != number || pr.State != "open" || pr.Merged:
		bad(ReasonNotOpen)
	case pr.Draft:
		bad(ReasonDraft)
	}
	if detail.Archived {
		bad(ReasonArchived)
	}
	// An absent setting is not consent: GitHub leaves it out for a token that cannot see it.
	if detail.AllowSquashMerge == nil || !*detail.AllowSquashMerge {
		bad(ReasonSquash)
	}
	if pr.HeadRepoMissing || pr.Head.RepoID == 0 {
		bad(ReasonHeadRepo)
	} else if pr.Head.RepoID != detail.ID || pr.Base.RepoID != detail.ID ||
		!strings.EqualFold(pr.Head.RepoFullName, detail.FullName) || !strings.EqualFold(pr.Base.RepoFullName, detail.FullName) {
		bad(ReasonRepository)
	}
	if pr.Base.Ref != detail.DefaultBranch {
		bad(ReasonBase)
	}
	if pr.Head.Ref == detail.DefaultBranch {
		bad(ReasonHeadIsDefault)
	}

	// The squash lands on the default branch's tip, not on the base the pull request recorded.
	if f.BaseSHA, err = gh.BranchTip(ctx, repo, detail.DefaultBranch); err != nil {
		return nil, readErr("read default branch", err)
	}

	// Files, a page at a time, up to the cap.
	complete := true
	for page := 1; ; page++ {
		files, more, err := gh.PullFiles(ctx, repo, number, page, 100)
		if err != nil {
			return nil, readErr("read changed files", err)
		}
		f.Files = append(f.Files, fileFacts(files)...)
		if len(f.Files) > MaxFiles {
			bad(ReasonTooManyFiles)
			complete = false
			break
		}
		if !more {
			break
		}
	}
	if complete {
		adds, dels := 0, 0
		for _, x := range f.Files {
			adds += x.Additions
			dels += x.Deletions
		}
		if len(f.Files) != pr.ChangedFiles || adds != pr.Additions || dels != pr.Deletions {
			bad(ReasonFilesIncomplete)
		}
		if _, err := protectedMatches(f.Files); err != nil {
			bad(ReasonInvalidPath)
		}
	}
	f.PathsDigest = pathsDigest(f.Files)

	behind := pr.MergeableState == "behind"
	if behind {
		bad(ReasonOutOfDate)
	}
	req, err := readRequirements(ctx, gh, repo, pr.Base.Ref, behind, &f)
	if err != nil {
		return nil, err
	}
	for _, reason := range req.Reasons {
		bad(reason)
	}
	if err := readChecks(ctx, gh, repo, &f, req.Required, now); err != nil {
		return nil, err
	}

	// The squash's own paths need a mergeable pull request on a current base; until GitHub has
	// answered, only the pull request's inventory is known and the evidence waits.
	if len(f.Unavailable) == 0 && pr.Mergeable != nil && *pr.Mergeable {
		inv, err := mergeInventory(ctx, gh, repo, f.BaseSHA, f.HeadSHA)
		if err != nil {
			return nil, err
		}
		f.MergeBaseSHA = inv.MergeBase
		if inv.Reason != "" {
			bad(inv.Reason)
		}
		f.MergeFiles = inv.Files
		if f.MergeFiles == nil {
			f.MergeFiles = []FileFact{}
		}
	}
	f.MergeFilesDigest = pathsDigest(f.MergeFiles)
	if m, err := protectedMatches(slices.Concat(f.Files, f.MergeFiles)); err == nil && m != nil {
		f.Protected = m
	}

	if err := refresh(ctx, gh, repo, number, pr, detail, f.BaseSHA); err != nil {
		return nil, err
	}
	slices.Sort(f.Unavailable)
	f.Unavailable = slices.Compact(f.Unavailable)
	return &Evidence{Facts: f, PR: *pr}, nil
}

// refresh reads the pull request, the repository and the default branch again; any difference means
// the evidence was gathered across a change and cannot be used.
func refresh(ctx context.Context, gh GitHub, repo github.Repo, number int, pr *github.PullRequestInfo, detail *github.MergeRepo, baseSHA string) error {
	fresh, err := gh.PullRequest(ctx, repo, number)
	if err != nil {
		return fmt.Errorf("read pull request again: %w", err)
	}
	if !samePullRequest(pr, fresh) {
		return fmt.Errorf("%w: the pull request", errStale)
	}
	again, err := gh.MergeSettings(ctx, repo)
	if err != nil {
		return readErr("read repository again", err)
	}
	if !sameRepository(detail, again) {
		return fmt.Errorf("%w: the repository", errStale)
	}
	tip, err := gh.BranchTip(ctx, repo, detail.DefaultBranch)
	if err != nil {
		return readErr("read default branch again", err)
	}
	if tip != baseSHA {
		return fmt.Errorf("%w: the default branch", errStale)
	}
	return nil
}

func samePullRequest(a, b *github.PullRequestInfo) bool {
	am, bm := a.Mergeable, b.Mergeable
	a2, b2 := *a, *b
	a2.Mergeable, b2.Mergeable = nil, nil
	return a2 == b2 && (am == nil) == (bm == nil) && (am == nil || *am == *bm)
}

func sameRepository(a, b *github.MergeRepo) bool {
	am, bm := a.AllowSquashMerge, b.AllowSquashMerge
	return a.RepoDetail == b.RepoDetail && (am == nil) == (bm == nil) && (am == nil || *am == *bm)
}

// readRequirements reads classic protection and the active rules and judges them. A refusal that
// GitHub answers with its plan message means the plan has neither feature; any other failure is an
// error, so a requirement that could not be read is never taken for none.
func readRequirements(ctx context.Context, gh GitHub, repo github.Repo, base string, behind bool, f *Facts) (*requirements, error) {
	req := &requirements{}
	prot, err := gh.BranchProtection(ctx, repo, base)
	switch {
	case err == nil:
		req.readProtection(prot, behind)
	case errors.Is(err, github.ErrPlanUnavailable):
		f.RulesUnavailable = append(f.RulesUnavailable, "protection")
	default:
		return nil, readErr("read branch protection", err)
	}
	rules, err := gh.BranchRules(ctx, repo, base)
	switch {
	case err == nil:
		req.readRules(rules, behind)
	case errors.Is(err, github.ErrPlanUnavailable):
		f.RulesUnavailable = append(f.RulesUnavailable, "rules")
	default:
		return nil, readErr("read branch rules", err)
	}
	slices.SortFunc(req.Required, func(a, b RequiredCheck) int {
		if c := strings.Compare(a.Context, b.Context); c != 0 {
			return c
		}
		return int(a.AppID - b.AppID)
	})
	req.Required = slices.Compact(req.Required)
	f.Required = append(f.Required, req.Required...)
	return req, nil
}

// readChecks adds the CI results at the head commit: the latest run of each (app, name), the
// latest status of each context, every suite that has not succeeded, and required checks nobody
// reported. A suite is judged on its own, so a failing suite cannot hide behind a passing run.
func readChecks(ctx context.Context, gh GitHub, repo github.Repo, f *Facts, required []RequiredCheck, now time.Time) error {
	suites, err := gh.CheckSuites(ctx, repo, f.HeadSHA)
	if err != nil {
		return readErr("read check suites", err)
	}
	runs, err := gh.CheckRuns(ctx, repo, f.HeadSHA)
	if err != nil {
		return readErr("read check runs", err)
	}
	statuses, err := gh.CommitStatuses(ctx, repo, f.HeadSHA)
	if err != nil {
		return readErr("read commit statuses", err)
	}

	type key struct {
		app  int64
		name string
	}
	listed := map[int64]bool{}
	for _, s := range suites {
		listed[s.ID] = true
	}
	latestRun := map[key]github.CheckRun{}
	suitesWithRuns := map[int64]bool{}
	for _, r := range runs {
		// A run of a suite GitHub did not list is an inconsistent reading, not a fact.
		if !listed[r.SuiteID] {
			return fmt.Errorf("read check runs: run %d belongs to an unlisted suite", r.ID)
		}
		suitesWithRuns[r.SuiteID] = true
		if k := (key{r.AppID, r.Name}); r.ID >= latestRun[k].ID {
			latestRun[k] = r
		}
	}
	latestStatus := map[string]github.CommitStatus{}
	for _, s := range statuses {
		if old, ok := latestStatus[s.Context]; !ok || s.CreatedAt.After(old.CreatedAt) || (s.CreatedAt.Equal(old.CreatedAt) && s.ID > old.ID) {
			latestStatus[s.Context] = s
		}
	}

	for k, r := range latestRun {
		f.Checks = append(f.Checks, CheckFact{Kind: "check_run", AppID: k.app, Context: k.name, Status: statusText(r.Status, r.Conclusion), Result: checkResult(r.Status, r.Conclusion)})
	}
	for name, s := range latestStatus {
		f.Checks = append(f.Checks, CheckFact{Kind: "commit_status", Context: name, Status: s.State, Result: statusResult(s.State)})
	}
	requiredApps := map[int64]bool{}
	for _, r := range required {
		requiredApps[r.AppID] = true
	}
	for _, s := range suites {
		if abandoned(s, suitesWithRuns, requiredApps, now) {
			f.IgnoredSuites++
			continue
		}
		if res := checkResult(s.Status, s.Conclusion); res != resultPassed {
			f.Checks = append(f.Checks, CheckFact{Kind: "suite", AppID: s.AppID, Context: s.AppSlug, Status: statusText(s.Status, s.Conclusion), Result: res})
		}
	}
	for _, r := range required {
		var matched bool
		for k := range latestRun {
			matched = matched || (k.name == r.Context && (r.AppID == 0 || k.app == r.AppID))
		}
		if _, ok := latestStatus[r.Context]; ok && r.AppID == 0 {
			matched = true
		}
		// A required check that ran is already a result above; one nobody reported, or only
		// another app reported, is pending.
		if !matched {
			f.Checks = append(f.Checks, CheckFact{Kind: "required_check", AppID: r.AppID, Context: r.Context, Status: "missing", Result: resultPending})
		}
	}
	slices.SortFunc(f.Checks, func(a, b CheckFact) int {
		return strings.Compare(fmt.Sprintf("%s\x00%s\x00%020d", a.Kind, a.Context, a.AppID), fmt.Sprintf("%s\x00%s\x00%020d", b.Kind, b.Context, b.AppID))
	})
	return nil
}

// abandoned reports whether a suite is a queued shell its app never filled: no runs, none expected,
// an app no rule requires, and older than the grace period.
func abandoned(s github.CheckSuite, withRuns, requiredApps map[int64]bool, now time.Time) bool {
	if s.Status != "queued" || s.Conclusion != "" || withRuns[s.ID] || s.AppID == 0 || requiredApps[s.AppID] ||
		(s.LatestCheckRunsCount != nil && *s.LatestCheckRunsCount != 0) {
		return false
	}
	created, err := time.Parse(time.RFC3339, s.CreatedAt)
	return err == nil && now.Sub(created) > abandonedSuiteGrace
}

func statusText(status, conclusion string) string {
	if status == "completed" && conclusion != "" {
		return conclusion
	}
	return status
}

func pathsDigest(files []FileFact) string {
	type p struct{ F, P, S string }
	list := make([]p, 0, len(files))
	for _, x := range files {
		list = append(list, p{x.Filename, x.PreviousFilename, x.Status})
	}
	slices.SortFunc(list, func(a, b p) int {
		return strings.Compare(a.F+"\x00"+a.P+"\x00"+a.S, b.F+"\x00"+b.P+"\x00"+b.S)
	})
	return digest(list)
}

func canonical(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}

func digest(v any) string {
	sum := sha256.Sum256(canonical(v))
	return hex.EncodeToString(sum[:])
}

// verdict is what an evaluation concludes from a fresh reading.
type verdict int

const (
	verdictReady verdict = iota
	verdictWait          // CI pending, or GitHub still computing mergeability
	verdictBlocked
	verdictSuperseded
)

// judge decides what to do with a candidate given fresh evidence and the facts it was prepared
// from. why is a stable detail for blocked and superseded.
func judge(ev *Evidence, want Facts, headSHA string) (verdict, string) {
	f := ev.Facts
	switch {
	case f.HeadSHA != headSHA:
		return verdictSuperseded, "head_moved"
	case ev.PR.Merged || ev.PR.State != "open":
		return verdictBlocked, "pull_request_not_open"
	case ev.PR.Mergeable != nil && (!*ev.PR.Mergeable || ev.PR.MergeableState == "dirty"):
		return verdictBlocked, "not_mergeable"
	case len(f.Unavailable) > 0:
		return verdictBlocked, "evidence_unavailable: " + strings.Join(f.Unavailable, ",")
	case ev.PR.Mergeable == nil:
		// The squash's inventory is not read until GitHub has answered.
		return verdictWait, "waiting for mergeability"
	case f.PathsDigest != want.PathsDigest || f.MergeFilesDigest != want.MergeFilesDigest || !slices.Equal(f.Protected, want.Protected):
		return verdictBlocked, "facts_changed"
	}
	c := countChecks(f.Checks)
	switch {
	case c.Failed > 0:
		return verdictBlocked, "checks_failed"
	case c.Pending > 0 || c.Results == 0:
		return verdictWait, "waiting for checks"
	}
	return verdictReady, ""
}

// readErr wraps a failed GitHub read. A 404 is stripped to text: only the pull request read may say
// the pull request is gone, and a missing commit or branch is a read to try again.
func readErr(what string, err error) error {
	if _, ok := github.StatusOf(err); ok {
		return errors.New(what + ": " + err.Error())
	}
	return fmt.Errorf("%s: %w", what, err)
}
