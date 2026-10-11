package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v88/github"
)

// This file is the pull-request and merge side of the client: the reads that build merge
// evidence, the one call that opens a PR, and the one call that merges it. The calls decode into
// small structs of their own, so the fields the product depends on are spelled out here, and every
// failure is reduced by [App.opaque]: status only, no upstream text.

// ErrIncomplete means a listing had more pages than the client reads. Evidence built from it
// would be partial, so callers treat it as unavailable.
var ErrIncomplete = errors.New("github: listing is longer than the client reads")

// ErrBranchName means a branch name cannot be sent as a request path.
var ErrBranchName = errors.New("github: unsupported branch name")

// StatusOf returns the HTTP status behind err when GitHub answered with one: the status of a
// [*StatusError], or 404 for [ErrNotFound]. It reports false for everything else (a transport
// failure, a timeout, a refused token), where it is not known whether GitHub acted.
func StatusOf(err error) (int, bool) {
	var se *StatusError
	switch {
	case errors.As(err, &se):
		return se.Status, true
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, true
	}
	return 0, false
}

// ErrPlanUnavailable means GitHub answered a branch-protection or ruleset read with its
// "upgrade to enable this feature" refusal: the repository's plan has no such feature, so there
// are no requirements to read. It is returned only for the first page of a read and only for
// GitHub's exact refusal text; every other 403 is a [*StatusError] and means the requirements
// could not be read.
var ErrPlanUnavailable = errors.New("github: feature unavailable on the repository's plan")

// planUnavailableMessage is the text of that refusal.
const planUnavailableMessage = "Upgrade to GitHub Pro or make this repository public to enable this feature."

// RepoDetail is the part of a repository's record that identifies it.
type RepoDetail struct {
	ID            int64
	FullName      string
	DefaultBranch string
	Archived      bool
}

// MergeRepo is a repository's record with its merge settings. AllowSquashMerge is nil when
// GitHub did not report the setting, which it does not for a token without push access: the
// caller must not assume it.
type MergeRepo struct {
	RepoDetail
	AllowSquashMerge *bool
}

// PRRef is one end of a pull request.
type PRRef struct {
	Ref          string
	SHA          string
	RepoID       int64
	RepoFullName string
}

// PullRequestInfo is a pull request as GitHub reports it. A listing leaves the detail fields
// (Mergeable, Additions and so on) at their zero values.
type PullRequestInfo struct {
	Number         int
	State          string
	Draft          bool
	Merged         bool
	MergeCommitSHA string
	// Mergeable is nil while GitHub is still working it out.
	Mergeable      *bool
	MergeableState string
	Title          string
	Body           string
	// BodyAvailable distinguishes an observed empty body from null or missing text.
	BodyAvailable   bool
	Additions       int
	Deletions       int
	ChangedFiles    int
	HTMLURL         string
	Head, Base      PRRef
	HeadRepoMissing bool
}

// PullFile is one changed file of a pull request.
type PullFile struct {
	Filename         string
	PreviousFilename string
	Status           string
	Additions        int
	Deletions        int
}

// CheckSuite is one check suite on a commit. AppID is zero for a suite with no app.
// LatestCheckRunsCount is nil when GitHub did not report it.
type CheckSuite struct {
	ID                   int64
	AppID                int64
	AppSlug              string
	HeadSHA              string
	Status               string
	Conclusion           string
	CreatedAt            string
	LatestCheckRunsCount *int
}

// CheckRun is one check run on a commit. SuiteID is the check suite it belongs to.
type CheckRun struct {
	ID         int64
	AppID      int64
	SuiteID    int64
	Name       string
	HeadSHA    string
	Status     string
	Conclusion string
}

// CommitStatus is one commit status (the older CI interface).
type CommitStatus struct {
	ID        int64
	Context   string
	State     string
	CreatedAt time.Time
}

// Protection is classic branch protection as GitHub returned it. Present is false when the branch
// has none. Raw is the answer unparsed, so the caller that judges it sees every field: the client
// does not decide which requirements matter.
type Protection struct {
	Present bool
	Raw     json.RawMessage
}

// BranchRule is one active ruleset rule that applies to a branch, as GitHub returned it.
// Parameters is nil when the rule has none. Malformed marks an entry that is not an object with a
// string type; nothing else about it is known.
type BranchRule struct {
	Type       string
	Parameters json.RawMessage
	Malformed  bool
}

// CompareCommit is one commit of a comparison with the commits it descends from.
type CompareCommit struct {
	SHA     string
	Parents []string
}

// Comparison is GitHub's answer to comparing two commits (base...head). Files are listed on the
// first page only and capped at [CompareFilesLimit] for the whole comparison; FilesListed is false
// when GitHub sent none.
type Comparison struct {
	Status       string
	AheadBy      int
	BehindBy     int
	BaseSHA      string
	MergeBaseSHA string
	Commits      []CompareCommit
	Files        []PullFile
	FilesListed  bool
}

// CompareFilesLimit is the most changed files GitHub lists for a comparison; a list at the limit
// may be incomplete.
const CompareFilesLimit = 300

// MergeResult is GitHub's answer to a merge request.
type MergeResult struct {
	Merged bool
	SHA    string
}

type refJSON struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repo"`
}

type pullJSON struct {
	Number         int       `json:"number"`
	State          string    `json:"state"`
	Draft          bool      `json:"draft"`
	Merged         bool      `json:"merged"`
	MergeCommitSHA *string   `json:"merge_commit_sha"`
	Mergeable      *bool     `json:"mergeable"`
	MergeableState string    `json:"mergeable_state"`
	Title          string    `json:"title"`
	Body           *pullBody `json:"body"`
	Additions      int       `json:"additions"`
	Deletions      int       `json:"deletions"`
	ChangedFiles   int       `json:"changed_files"`
	HTMLURL        string    `json:"html_url"`
	Head           refJSON   `json:"head"`
	Base           refJSON   `json:"base"`
}

func (p pullJSON) info() PullRequestInfo {
	conv := func(r refJSON) (PRRef, bool) {
		out := PRRef{Ref: r.Ref, SHA: r.SHA}
		if r.Repo == nil {
			return out, true
		}
		out.RepoID, out.RepoFullName = r.Repo.ID, r.Repo.FullName
		return out, false
	}
	head, missing := conv(p.Head)
	base, _ := conv(p.Base)
	out := PullRequestInfo{
		Number: p.Number, State: p.State, Draft: p.Draft, Merged: p.Merged,
		Mergeable: p.Mergeable, MergeableState: p.MergeableState, Title: p.Title,
		Additions: p.Additions, Deletions: p.Deletions, ChangedFiles: p.ChangedFiles,
		HTMLURL: p.HTMLURL, Head: head, Base: base, HeadRepoMissing: missing,
	}
	if p.Body != nil {
		out.Body, out.BodyAvailable = string(*p.Body), true
	}
	if p.MergeCommitSHA != nil {
		out.MergeCommitSHA = *p.MergeCommitSHA
	}
	return out
}

// call sends one request with a token scoped to repo and perms and decodes a JSON answer into
// out (nil for none). A rate limit comes back as status 429, never 403, so callers can tell it from
// a refusal.
func (a *App) call(ctx context.Context, repo Repo, perms Permissions, method, path string, in, out any) error {
	if err := a.callRaw(ctx, repo, perms, method, path, in, out); err != nil {
		return a.opaque(err)
	}
	return nil
}

// callRaw is call without reducing the error, for the reads that look at GitHub's own message.
// Its result must go through opaque before it leaves the package; a rate limit is already a 429.
func (a *App) callRaw(ctx context.Context, repo Repo, perms Permissions, method, path string, in, out any) error {
	c, err := a.client(ctx, repo, perms)
	if err != nil {
		return err
	}
	req, err := c.NewRequest(ctx, method, path, in)
	if err != nil {
		return ErrUnavailable
	}
	if _, err := c.Do(req, out); err != nil {
		var (
			rl *gh.RateLimitError
			ab *gh.AbuseRateLimitError
		)
		if errors.As(err, &rl) || errors.As(err, &ab) {
			return &StatusError{Status: http.StatusTooManyRequests}
		}
		return err
	}
	return nil
}

// planRefusal reports whether err is GitHub's plan refusal: a 403 whose message is exactly
// [planUnavailableMessage]. Any other 403, a rate limit included, is not.
func planRefusal(err error) bool {
	var er *gh.ErrorResponse
	return errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusForbidden &&
		er.Message == planUnavailableMessage
}

func repoPath(repo Repo, rest ...string) string {
	return strings.Join(append([]string{"repos", repo.Owner, repo.Name}, rest...), "/")
}

// branchPath renders a branch name as path segments. Each segment must be plain: a name that
// needs escaping is refused, so the request path never carries a percent escape.
func branchPath(branch string) (string, error) {
	if branch == "" || strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") {
		return "", ErrBranchName
	}
	for _, seg := range strings.Split(branch, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.TrimSpace(seg) != seg {
			return "", ErrBranchName
		}
		for _, r := range seg {
			ok := r == '.' || r == '_' || r == '-' || r == '+' || r == '@' || r == '=' || r == ',' ||
				(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
			if !ok {
				return "", ErrBranchName
			}
		}
	}
	return branch, nil
}

// RepoDetail reads the repository's identity: id, name, default branch and whether it is archived.
func (a *App) RepoDetail(ctx context.Context, repo Repo) (*RepoDetail, error) {
	var out struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Archived      bool   `json:"archived"`
	}
	if err := a.call(ctx, repo, Permissions{"metadata": Read}, http.MethodGet, repoPath(repo), nil, &out); err != nil {
		return nil, err
	}
	if out.ID <= 0 || out.FullName == "" || out.DefaultBranch == "" {
		return nil, ErrUnavailable
	}
	return &RepoDetail{ID: out.ID, FullName: out.FullName, DefaultBranch: out.DefaultBranch, Archived: out.Archived}, nil
}

// MergeSettings reads the repository record with its merge settings. GitHub leaves
// allow_squash_merge out of the answer for a token without Contents access, so this read asks for
// Contents write (the access the merge itself needs); AllowSquashMerge is nil when the answer still
// lacks it, and the caller must then refuse rather than assume.
func (a *App) MergeSettings(ctx context.Context, repo Repo) (*MergeRepo, error) {
	var out struct {
		ID               int64  `json:"id"`
		FullName         string `json:"full_name"`
		DefaultBranch    string `json:"default_branch"`
		AllowSquashMerge *bool  `json:"allow_squash_merge"`
		Archived         bool   `json:"archived"`
	}
	if err := a.call(ctx, repo, Permissions{"contents": Write, "metadata": Read}, http.MethodGet, repoPath(repo), nil, &out); err != nil {
		return nil, err
	}
	if out.ID <= 0 || out.FullName == "" || out.DefaultBranch == "" {
		return nil, ErrUnavailable
	}
	return &MergeRepo{
		RepoDetail:       RepoDetail{ID: out.ID, FullName: out.FullName, DefaultBranch: out.DefaultBranch, Archived: out.Archived},
		AllowSquashMerge: out.AllowSquashMerge,
	}, nil
}

// BranchTip returns the commit a branch points at. A missing branch is [ErrNotFound].
func (a *App) BranchTip(ctx context.Context, repo Repo, branch string) (string, error) {
	p, err := branchPath(branch)
	if err != nil {
		return "", err
	}
	var out struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := a.call(ctx, repo, Permissions{"contents": Read}, http.MethodGet, repoPath(repo, "branches", p), nil, &out); err != nil {
		return "", err
	}
	if out.Name != branch || !isSHA(out.Commit.SHA) {
		return "", ErrUnavailable
	}
	return out.Commit.SHA, nil
}

func isSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

const (
	listPerPage  = 100
	listMaxPages = 10
)

// PullRequestsForHead lists every pull request, open or not, whose head is owner:head and whose
// base is base. A list of more than ten full pages is [ErrIncomplete]. Link headers are never
// followed; each page is asked for by number.
func (a *App) PullRequestsForHead(ctx context.Context, repo Repo, head, base string) ([]PullRequestInfo, error) {
	var all []PullRequestInfo
	for page := 1; page <= listMaxPages; page++ {
		q := url.Values{
			"state": {"all"}, "head": {repo.Owner + ":" + head}, "base": {base},
			"per_page": {strconv.Itoa(listPerPage)}, "page": {strconv.Itoa(page)},
		}
		var got []pullJSON
		if err := a.call(ctx, repo, Permissions{"pull_requests": Read}, http.MethodGet, repoPath(repo, "pulls")+"?"+q.Encode(), nil, &got); err != nil {
			return nil, err
		}
		if len(got) > listPerPage {
			return nil, ErrUnavailable
		}
		for _, p := range got {
			all = append(all, p.info())
		}
		if len(got) < listPerPage {
			return all, nil
		}
	}
	return nil, ErrIncomplete
}

// CreatePullRequest opens a pull request. It is sent at most once per intent by the caller; this
// method never retries. A refusal comes back as a [*StatusError] with the 4xx status.
func (a *App) CreatePullRequest(ctx context.Context, repo Repo, head, base, title, body string) (*PullRequestInfo, error) {
	in := map[string]any{"head": head, "base": base, "title": title, "body": body}
	var out pullJSON
	if err := a.call(ctx, repo, Permissions{"pull_requests": Write}, http.MethodPost, repoPath(repo, "pulls"), in, &out); err != nil {
		return nil, err
	}
	info := out.info()
	if info.Number <= 0 {
		return nil, ErrUnavailable
	}
	return &info, nil
}

// PullRequest reads one pull request with its detail fields.
func (a *App) PullRequest(ctx context.Context, repo Repo, number int) (*PullRequestInfo, error) {
	var out pullJSON
	if err := a.call(ctx, repo, Permissions{"pull_requests": Read}, http.MethodGet, repoPath(repo, "pulls", strconv.Itoa(number)), nil, &out); err != nil {
		return nil, err
	}
	info := out.info()
	if info.Number != number {
		return nil, ErrUnavailable
	}
	return &info, nil
}

// PullFiles reads one page of a pull request's changed files, perPage at a time, and says whether
// a further page exists. The page size stays small because each file may carry a patch and every
// answer is capped.
func (a *App) PullFiles(ctx context.Context, repo Repo, number, page, perPage int) ([]PullFile, bool, error) {
	q := url.Values{"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
	var got []struct {
		Filename         string  `json:"filename"`
		PreviousFilename *string `json:"previous_filename"`
		Status           string  `json:"status"`
		Additions        int     `json:"additions"`
		Deletions        int     `json:"deletions"`
	}
	path := repoPath(repo, "pulls", strconv.Itoa(number), "files") + "?" + q.Encode()
	if err := a.call(ctx, repo, Permissions{"pull_requests": Read}, http.MethodGet, path, nil, &got); err != nil {
		return nil, false, err
	}
	if len(got) > perPage {
		return nil, false, ErrUnavailable
	}
	out := make([]PullFile, 0, len(got))
	for _, f := range got {
		pf := PullFile{Filename: f.Filename, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions}
		if f.PreviousFilename != nil {
			pf.PreviousFilename = *f.PreviousFilename
		}
		out = append(out, pf)
	}
	return out, len(got) == perPage, nil
}

// MergePullRequest asks GitHub to squash-merge the pull request at exactly sha. It is the only
// call that changes a branch, and the caller writes its intent first and never sends it twice for
// one intent. A refusal is a [*StatusError] (or [ErrNotFound]); anything with no status is
// unknown.
func (a *App) MergePullRequest(ctx context.Context, repo Repo, number int, sha string) (*MergeResult, error) {
	in := map[string]any{"sha": sha, "merge_method": "squash"}
	var out struct {
		Merged bool   `json:"merged"`
		SHA    string `json:"sha"`
	}
	path := repoPath(repo, "pulls", strconv.Itoa(number), "merge")
	if err := a.call(ctx, repo, Permissions{"contents": Write, "pull_requests": Write}, http.MethodPut, path, in, &out); err != nil {
		return nil, err
	}
	return &MergeResult{Merged: out.Merged, SHA: out.SHA}, nil
}

// checkPages reads a counted listing (check suites or runs) a page at a time. GitHub's total_count
// must be present, the same on every page, within the cap, and equal to what was listed: a listing
// that disagrees with itself is not evidence.
func checkPages[T any](path, key string, max int, get func(path string, out any) error, add func(T) error) error {
	total := -1
	listed := 0
	for page := 1; page <= max/listPerPage+1; page++ {
		var got map[string]json.RawMessage
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		if err := get(path+fmt.Sprintf("%sper_page=%d&page=%d", sep, listPerPage, page), &got); err != nil {
			return err
		}
		var count *int
		if raw, ok := got["total_count"]; ok {
			if err := json.Unmarshal(raw, &count); err != nil {
				return ErrUnavailable
			}
		}
		if count == nil || *count < 0 || *count > max || (total >= 0 && total != *count) {
			return ErrUnavailable
		}
		total = *count
		var items []T
		if err := json.Unmarshal(got[key], &items); err != nil || len(items) > listPerPage {
			return ErrUnavailable
		}
		for _, it := range items {
			if err := add(it); err != nil {
				return err
			}
		}
		listed += len(items)
		if listed > max {
			return ErrUnavailable
		}
		if len(items) < listPerPage {
			if listed != total {
				return ErrUnavailable
			}
			return nil
		}
	}
	return ErrIncomplete
}

// maxChecks caps a check listing; a commit with more is not read.
const maxChecks = 1000

// CheckSuites lists the check suites on a commit. A suite that GitHub reports without an id, with a
// head other than sha, or twice is [ErrUnavailable].
func (a *App) CheckSuites(ctx context.Context, repo Repo, sha string) ([]CheckSuite, error) {
	if !isSHA(sha) {
		return nil, ErrUnavailable
	}
	type suiteJSON struct {
		ID                   int64   `json:"id"`
		HeadSHA              string  `json:"head_sha"`
		Status               *string `json:"status"`
		Conclusion           *string `json:"conclusion"`
		CreatedAt            *string `json:"created_at"`
		LatestCheckRunsCount *int    `json:"latest_check_runs_count"`
		App                  *struct {
			ID   int64  `json:"id"`
			Slug string `json:"slug"`
		} `json:"app"`
	}
	var all []CheckSuite
	seen := map[int64]bool{}
	get := func(path string, out any) error {
		return a.call(ctx, repo, Permissions{"checks": Read}, http.MethodGet, path, nil, out)
	}
	err := checkPages(repoPath(repo, "commits", sha, "check-suites"), "check_suites", maxChecks-1, get, func(s suiteJSON) error {
		if s.ID <= 0 || s.HeadSHA != sha || s.Status == nil || seen[s.ID] || (s.App != nil && s.App.ID <= 0) {
			return ErrUnavailable
		}
		if s.LatestCheckRunsCount != nil && *s.LatestCheckRunsCount < 0 {
			return ErrUnavailable
		}
		seen[s.ID] = true
		cs := CheckSuite{ID: s.ID, HeadSHA: s.HeadSHA, Status: *s.Status, Conclusion: deref(s.Conclusion), CreatedAt: deref(s.CreatedAt), LatestCheckRunsCount: s.LatestCheckRunsCount}
		if s.App != nil {
			cs.AppID, cs.AppSlug = s.App.ID, s.App.Slug
		}
		all = append(all, cs)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// CheckRuns lists the check runs on a commit, including earlier runs of the same check. A run with
// no app, no suite, a head other than sha, or a repeated id is [ErrUnavailable].
func (a *App) CheckRuns(ctx context.Context, repo Repo, sha string) ([]CheckRun, error) {
	if !isSHA(sha) {
		return nil, ErrUnavailable
	}
	type runJSON struct {
		ID         int64   `json:"id"`
		Name       string  `json:"name"`
		HeadSHA    string  `json:"head_sha"`
		Status     *string `json:"status"`
		Conclusion *string `json:"conclusion"`
		App        *struct {
			ID int64 `json:"id"`
		} `json:"app"`
		Suite *struct {
			ID int64 `json:"id"`
		} `json:"check_suite"`
	}
	var all []CheckRun
	seen := map[int64]bool{}
	get := func(path string, out any) error {
		return a.call(ctx, repo, Permissions{"checks": Read}, http.MethodGet, path, nil, out)
	}
	err := checkPages(repoPath(repo, "commits", sha, "check-runs")+"?filter=all", "check_runs", maxChecks, get, func(r runJSON) error {
		if r.ID <= 0 || r.Name == "" || r.HeadSHA != sha || r.Status == nil || seen[r.ID] ||
			r.App == nil || r.App.ID <= 0 || r.Suite == nil || r.Suite.ID <= 0 {
			return ErrUnavailable
		}
		seen[r.ID] = true
		all = append(all, CheckRun{ID: r.ID, Name: r.Name, HeadSHA: r.HeadSHA, Status: *r.Status, Conclusion: deref(r.Conclusion), AppID: r.App.ID, SuiteID: r.Suite.ID})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// CommitStatuses lists the commit statuses on a commit. A status without an id, context, state or
// creation time, or a repeated id, is [ErrUnavailable].
func (a *App) CommitStatuses(ctx context.Context, repo Repo, sha string) ([]CommitStatus, error) {
	if !isSHA(sha) {
		return nil, ErrUnavailable
	}
	var all []CommitStatus
	seen := map[int64]bool{}
	for page := 1; page <= listMaxPages+1; page++ {
		var got []struct {
			ID        int64  `json:"id"`
			Context   string `json:"context"`
			State     string `json:"state"`
			CreatedAt string `json:"created_at"`
		}
		path := repoPath(repo, "commits", sha, "statuses") + fmt.Sprintf("?per_page=%d&page=%d", listPerPage, page)
		if err := a.call(ctx, repo, Permissions{"statuses": Read}, http.MethodGet, path, nil, &got); err != nil {
			return nil, err
		}
		if len(got) > listPerPage {
			return nil, ErrUnavailable
		}
		for _, s := range got {
			created, err := time.Parse(time.RFC3339, s.CreatedAt)
			if s.ID <= 0 || s.Context == "" || s.State == "" || err != nil || seen[s.ID] {
				return nil, ErrUnavailable
			}
			seen[s.ID] = true
			all = append(all, CommitStatus{ID: s.ID, Context: s.Context, State: s.State, CreatedAt: created})
		}
		if len(all) > maxChecks {
			return nil, ErrUnavailable
		}
		if len(got) < listPerPage {
			return all, nil
		}
	}
	return nil, ErrIncomplete
}

// BranchProtection reads classic branch protection and hands back GitHub's answer unparsed in Raw.
// A branch with none is Present false, not an error. A 403 that is GitHub's plan refusal (see
// [ErrPlanUnavailable]) is that error; every other failure, any other 403 included, is the status
// error, and means the requirements could not be read.
func (a *App) BranchProtection(ctx context.Context, repo Repo, branch string) (*Protection, error) {
	p, err := branchPath(branch)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	err = a.callRaw(ctx, repo, Permissions{"administration": Read}, http.MethodGet, repoPath(repo, "branches", p, "protection"), nil, &raw)
	switch {
	case err == nil:
		return &Protection{Present: true, Raw: raw}, nil
	case planRefusal(err):
		return nil, ErrPlanUnavailable
	}
	if err = a.opaque(err); errors.Is(err, ErrNotFound) {
		return &Protection{}, nil
	}
	return nil, err
}

// BranchRules reads the active ruleset rules that apply to a branch, as GitHub returned them. The
// plan refusal is [ErrPlanUnavailable] on the first page only; anywhere else it is a status error.
func (a *App) BranchRules(ctx context.Context, repo Repo, branch string) ([]BranchRule, error) {
	p, err := branchPath(branch)
	if err != nil || strings.Contains(p, "/") {
		return nil, ErrBranchName
	}
	var all []BranchRule
	for page := 1; page <= listMaxPages+1; page++ {
		var got []json.RawMessage
		path := repoPath(repo, "rules", "branches", p) + fmt.Sprintf("?per_page=%d&page=%d", listPerPage, page)
		err := a.callRaw(ctx, repo, Permissions{"metadata": Read}, http.MethodGet, path, nil, &got)
		switch {
		case err == nil:
		case page == 1 && planRefusal(err):
			return nil, ErrPlanUnavailable
		default:
			return nil, a.opaque(err)
		}
		if len(got) > listPerPage {
			return nil, ErrUnavailable
		}
		for _, raw := range got {
			var r struct {
				Type       *string         `json:"type"`
				Parameters json.RawMessage `json:"parameters"`
			}
			if json.Unmarshal(raw, &r) != nil || r.Type == nil || *r.Type == "" {
				all = append(all, BranchRule{Malformed: true})
				continue
			}
			rule := BranchRule{Type: *r.Type}
			if len(r.Parameters) > 0 && string(r.Parameters) != "null" {
				rule.Parameters = r.Parameters
			}
			all = append(all, rule)
		}
		if len(all) > maxChecks {
			return nil, ErrUnavailable
		}
		if len(got) < listPerPage {
			return all, nil
		}
	}
	return nil, ErrIncomplete
}

// Compare compares two commits, base...head, reading one page of the commits in between. Files
// are listed on the first page only. Every commit id in the answer must be a full SHA.
func (a *App) Compare(ctx context.Context, repo Repo, base, head string, perPage, page int) (*Comparison, error) {
	if !isSHA(base) || !isSHA(head) || perPage < 1 || perPage > listPerPage || page < 1 {
		return nil, ErrUnavailable
	}
	var out struct {
		Status     string `json:"status"`
		AheadBy    *int   `json:"ahead_by"`
		BehindBy   *int   `json:"behind_by"`
		BaseCommit struct {
			SHA string `json:"sha"`
		} `json:"base_commit"`
		MergeBase struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
		Commits []struct {
			SHA     string `json:"sha"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		} `json:"commits"`
		Files []struct {
			Filename         string  `json:"filename"`
			PreviousFilename *string `json:"previous_filename"`
			Status           string  `json:"status"`
			Additions        *int    `json:"additions"`
			Deletions        *int    `json:"deletions"`
		} `json:"files"`
	}
	path := repoPath(repo, "compare", base+"..."+head) + fmt.Sprintf("?per_page=%d&page=%d", perPage, page)
	if err := a.call(ctx, repo, Permissions{"contents": Read}, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if out.AheadBy == nil || out.BehindBy == nil || *out.AheadBy < 0 || *out.BehindBy < 0 || len(out.Status) > 32 ||
		!isSHA(out.BaseCommit.SHA) || !isSHA(out.MergeBase.SHA) || len(out.Commits) > listPerPage || len(out.Files) > CompareFilesLimit {
		return nil, ErrUnavailable
	}
	c := &Comparison{
		Status: out.Status, AheadBy: *out.AheadBy, BehindBy: *out.BehindBy,
		BaseSHA: out.BaseCommit.SHA, MergeBaseSHA: out.MergeBase.SHA, FilesListed: out.Files != nil,
	}
	for _, x := range out.Commits {
		cc := CompareCommit{SHA: x.SHA}
		if !isSHA(x.SHA) {
			return nil, ErrUnavailable
		}
		for _, p := range x.Parents {
			if !isSHA(p.SHA) {
				return nil, ErrUnavailable
			}
			cc.Parents = append(cc.Parents, p.SHA)
		}
		c.Commits = append(c.Commits, cc)
	}
	for _, f := range out.Files {
		if f.Filename == "" || f.Status == "" || f.Additions == nil || f.Deletions == nil || *f.Additions < 0 || *f.Deletions < 0 ||
			(f.PreviousFilename != nil && *f.PreviousFilename == "") {
			return nil, ErrUnavailable
		}
		pf := PullFile{Filename: f.Filename, Status: f.Status, Additions: *f.Additions, Deletions: *f.Deletions}
		if f.PreviousFilename != nil {
			pf.PreviousFilename = *f.PreviousFilename
		}
		c.Files = append(c.Files, pf)
	}
	return c, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
