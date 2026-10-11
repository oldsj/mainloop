package merge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oldsj/mainloop/server/internal/github"
)

// GitHub is the part of the GitHub App that merging uses.
type GitHub interface {
	RepoDetail(ctx context.Context, repo github.Repo) (*github.RepoDetail, error)
	PullRequest(ctx context.Context, repo github.Repo, number int) (*github.PullRequestInfo, error)
	PullFiles(ctx context.Context, repo github.Repo, number, page, perPage int) ([]github.PullFile, bool, error)
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

// FileFact is one changed file as frozen in a proposal.
type FileFact struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename,omitempty"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
}

// CheckFact is one CI result as frozen in a proposal: a check run, a commit status, a suite
// without runs, or a required check that has no result (yet).
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
	Version      int              `json:"version"`
	Repository   string           `json:"repository"`
	RepositoryID int64            `json:"repository_id"`
	PRNumber     int              `json:"pr_number"`
	Title        string           `json:"title"`
	Body         string           `json:"body"`
	Head         string           `json:"head"`
	HeadSHA      string           `json:"head_sha"`
	Base         string           `json:"base"`
	BaseSHA      string           `json:"base_sha"`
	Files        []FileFact       `json:"files"`
	Additions    int              `json:"additions"`
	Deletions    int              `json:"deletions"`
	PathsDigest  string           `json:"paths_digest"`
	Policy       string           `json:"policy"`
	PolicyVer    int              `json:"policy_version"`
	GlobsVersion int              `json:"globs_version"`
	Protected    []ProtectedMatch `json:"protected"`
	Route        string           `json:"route"`
	Checks       []CheckFact      `json:"checks"`
	// IgnoredSuites counts finished, passing suites that had no check runs.
	IgnoredSuites int `json:"ignored_suites"`
	// Required lists the required status checks of branch protection and rulesets.
	Required []string `json:"required"`
	// RulesUnavailable names the reads that answered 403, which GitHub does for repositories whose
	// plan has no branch protection or rulesets: "protection" and/or "rules".
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

func checkResult(status, conclusion string) string {
	if status != "completed" {
		return resultPending
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return resultPassed
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

// gather reads the pull request, its files and its CI facts. A GitHub failure other than the
// plan 403s is an error, to be retried; a pull request that cannot be merged on principle is
// reported in Facts.Unavailable instead.
func gather(ctx context.Context, gh GitHub, repo github.Repo, number int, now time.Time) (*Evidence, error) {
	pr, err := gh.PullRequest(ctx, repo, number)
	if err != nil {
		return nil, fmt.Errorf("read pull request: %w", err)
	}
	detail, err := gh.RepoDetail(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("read repository: %w", err)
	}
	f := Facts{
		Version: 1, Repository: detail.FullName, RepositoryID: detail.ID, PRNumber: pr.Number,
		Title: pr.Title, Body: pr.Body, Head: pr.Head.Ref, HeadSHA: pr.Head.SHA, Base: pr.Base.Ref,
		BaseSHA: pr.Base.SHA, Additions: pr.Additions, Deletions: pr.Deletions,
		GlobsVersion: GlobsVersion, CapturedAt: now.UTC().Truncate(time.Second),
		Files: []FileFact{}, Protected: []ProtectedMatch{}, Checks: []CheckFact{}, Required: []string{},
		RulesUnavailable: []string{}, Unavailable: []string{},
	}
	bad := func(reason string) { f.Unavailable = append(f.Unavailable, reason) }

	switch {
	case pr.State != "open" || pr.Merged:
		bad(ReasonNotOpen)
	case pr.Draft:
		bad(ReasonDraft)
	}
	if detail.Archived {
		bad(ReasonArchived)
	}
	if !detail.AllowSquashMerge {
		bad(ReasonSquash)
	}
	if pr.HeadRepoMissing || pr.Head.RepoID == 0 {
		bad(ReasonHeadRepo)
	} else if pr.Head.RepoID != detail.ID || pr.Base.RepoID != detail.ID {
		bad(ReasonRepository)
	}
	if pr.Base.Ref != detail.DefaultBranch {
		bad(ReasonBase)
	}

	// Files, a page at a time, up to the cap.
	complete := true
	for page := 1; ; page++ {
		files, more, err := gh.PullFiles(ctx, repo, number, page, 100)
		if err != nil {
			return nil, fmt.Errorf("read changed files: %w", err)
		}
		for _, x := range files {
			f.Files = append(f.Files, FileFact{
				Filename: x.Filename, PreviousFilename: x.PreviousFilename, Status: x.Status,
				Additions: x.Additions, Deletions: x.Deletions,
			})
		}
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
		m, err := protectedMatches(f.Files)
		if err != nil {
			bad(ReasonInvalidPath)
		}
		if m != nil {
			f.Protected = m
		}
	}
	f.PathsDigest = pathsDigest(f.Files)

	// Required checks: classic protection and rulesets. A 403 here means the plan has neither;
	// that is decided by the status code and the endpoint alone, never by the message.
	required := map[string]bool{}
	prot, err := gh.BranchProtection(ctx, repo, pr.Base.Ref)
	switch st, ok := github.StatusOf(err); {
	case err == nil:
		for _, c := range prot.RequiredChecks {
			required[c] = true
		}
	case ok && st == 403:
		f.RulesUnavailable = append(f.RulesUnavailable, "protection")
	default:
		return nil, fmt.Errorf("read branch protection: %w", err)
	}
	rules, err := gh.BranchRules(ctx, repo, pr.Base.Ref)
	switch st, ok := github.StatusOf(err); {
	case err == nil:
		for _, r := range rules {
			if r.Type == "merge_queue" {
				bad(ReasonMergeQueue)
			}
			for _, c := range r.RequiredChecks {
				required[c] = true
			}
		}
	case ok && st == 403:
		f.RulesUnavailable = append(f.RulesUnavailable, "rules")
	default:
		return nil, fmt.Errorf("read branch rules: %w", err)
	}
	for c := range required {
		f.Required = append(f.Required, c)
	}
	slices.Sort(f.Required)

	if err := readChecks(ctx, gh, repo, &f); err != nil {
		return nil, err
	}
	slices.Sort(f.Unavailable)
	f.Unavailable = slices.Compact(f.Unavailable)
	return &Evidence{Facts: f, PR: *pr}, nil
}

// readChecks adds the CI results at the head commit: the latest run of each (app, name), the
// latest status of each context, suites that have no runs, and required checks nobody reported.
func readChecks(ctx context.Context, gh GitHub, repo github.Repo, f *Facts) error {
	suites, err := gh.CheckSuites(ctx, repo, f.HeadSHA)
	if err != nil {
		return fmt.Errorf("read check suites: %w", err)
	}
	runs, err := gh.CheckRuns(ctx, repo, f.HeadSHA)
	if err != nil {
		return fmt.Errorf("read check runs: %w", err)
	}
	statuses, err := gh.CommitStatuses(ctx, repo, f.HeadSHA)
	if err != nil {
		return fmt.Errorf("read commit statuses: %w", err)
	}

	type key struct {
		app  int64
		name string
	}
	latestRun := map[key]github.CheckRun{}
	appsWithRuns := map[int64]bool{}
	for _, r := range runs {
		appsWithRuns[r.AppID] = true
		if k := (key{r.AppID, r.Name}); r.ID >= latestRun[k].ID {
			latestRun[k] = r
		}
	}
	latestStatus := map[string]github.CommitStatus{}
	for _, s := range statuses {
		if s.ID >= latestStatus[s.Context].ID {
			latestStatus[s.Context] = s
		}
	}

	have := map[string]string{} // check name -> result, for required checks
	for k, r := range latestRun {
		res := checkResult(r.Status, r.Conclusion)
		f.Checks = append(f.Checks, CheckFact{Kind: "check_run", AppID: k.app, Context: k.name, Status: statusText(r.Status, r.Conclusion), Result: res})
		have[k.name] = worse(have[k.name], res)
	}
	for ctxName, s := range latestStatus {
		res := statusResult(s.State)
		f.Checks = append(f.Checks, CheckFact{Kind: "commit_status", Context: ctxName, Status: s.State, Result: res})
		have[ctxName] = worse(have[ctxName], res)
	}
	for _, s := range suites {
		if appsWithRuns[s.AppID] {
			continue
		}
		res := checkResult(s.Status, s.Conclusion)
		if res == resultPassed {
			f.IgnoredSuites++
			continue
		}
		f.Checks = append(f.Checks, CheckFact{Kind: "suite", AppID: s.AppID, Context: s.AppSlug, Status: statusText(s.Status, s.Conclusion), Result: res})
	}
	for _, name := range f.Required {
		// A required check that ran is already a result above; one nobody reported is pending.
		if _, ok := have[name]; !ok {
			f.Checks = append(f.Checks, CheckFact{Kind: "required_check", Context: name, Status: "missing", Result: resultPending})
		}
	}
	slices.SortFunc(f.Checks, func(a, b CheckFact) int {
		return strings.Compare(fmt.Sprintf("%s\x00%s\x00%020d", a.Kind, a.Context, a.AppID), fmt.Sprintf("%s\x00%s\x00%020d", b.Kind, b.Context, b.AppID))
	})
	return nil
}

func statusText(status, conclusion string) string {
	if status == "completed" && conclusion != "" {
		return conclusion
	}
	return status
}

// worse combines two results of one name: failed beats pending beats passed.
func worse(a, b string) string {
	rank := map[string]int{"": 0, resultPassed: 1, resultPending: 2, resultFailed: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
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
	case len(f.Unavailable) > 0:
		return verdictBlocked, "evidence_unavailable: " + strings.Join(f.Unavailable, ",")
	case f.PathsDigest != want.PathsDigest || !slices.Equal(f.Protected, want.Protected):
		return verdictBlocked, "facts_changed"
	}
	c := countChecks(f.Checks)
	switch {
	case c.Failed > 0:
		return verdictBlocked, "checks_failed"
	case ev.PR.Mergeable != nil && !*ev.PR.Mergeable:
		return verdictBlocked, "not_mergeable"
	case c.Pending > 0 || c.Results == 0 || ev.PR.Mergeable == nil:
		return verdictWait, "waiting for checks"
	}
	return verdictReady, ""
}
