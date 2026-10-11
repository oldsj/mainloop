package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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

// RepoDetail is the part of a repository's record that merging depends on.
type RepoDetail struct {
	ID               int64
	FullName         string
	DefaultBranch    string
	AllowSquashMerge bool
	Archived         bool
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
	Mergeable       *bool
	MergeableState  string
	Title           string
	Body            string
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

// CheckSuite is one check suite on a commit.
type CheckSuite struct {
	ID         int64
	AppID      int64
	AppSlug    string
	HeadSHA    string
	Status     string
	Conclusion string
}

// CheckRun is one check run on a commit.
type CheckRun struct {
	ID         int64
	AppID      int64
	Name       string
	HeadSHA    string
	Status     string
	Conclusion string
}

// CommitStatus is one commit status (the older CI interface).
type CommitStatus struct {
	ID      int64
	Context string
	State   string
}

// Protection is what classic branch protection asks for. Present is false when the branch has
// none. RequiredChecks lists every required status check by context name.
type Protection struct {
	Present        bool
	RequiredChecks []string
}

// BranchRule is one active ruleset rule that applies to a branch.
type BranchRule struct {
	Type           string
	RequiredChecks []string
}

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
	Number         int     `json:"number"`
	State          string  `json:"state"`
	Draft          bool    `json:"draft"`
	Merged         bool    `json:"merged"`
	MergeCommitSHA *string `json:"merge_commit_sha"`
	Mergeable      *bool   `json:"mergeable"`
	MergeableState string  `json:"mergeable_state"`
	Title          string  `json:"title"`
	Body           *string `json:"body"`
	Additions      int     `json:"additions"`
	Deletions      int     `json:"deletions"`
	ChangedFiles   int     `json:"changed_files"`
	HTMLURL        string  `json:"html_url"`
	Head           refJSON `json:"head"`
	Base           refJSON `json:"base"`
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
		out.Body = *p.Body
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
	c, err := a.client(ctx, repo, perms)
	if err != nil {
		return a.opaque(err)
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
		return a.opaque(err)
	}
	return nil
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

// RepoDetail reads the repository record merging depends on.
func (a *App) RepoDetail(ctx context.Context, repo Repo) (*RepoDetail, error) {
	var out struct {
		ID               int64  `json:"id"`
		FullName         string `json:"full_name"`
		DefaultBranch    string `json:"default_branch"`
		AllowSquashMerge *bool  `json:"allow_squash_merge"`
		Archived         bool   `json:"archived"`
	}
	if err := a.call(ctx, repo, Permissions{"metadata": Read}, http.MethodGet, repoPath(repo), nil, &out); err != nil {
		return nil, err
	}
	if out.ID <= 0 || out.FullName == "" || out.DefaultBranch == "" {
		return nil, ErrUnavailable
	}
	// A token without push access is not told; absent means the repository takes the default.
	squash := out.AllowSquashMerge == nil || *out.AllowSquashMerge
	return &RepoDetail{ID: out.ID, FullName: out.FullName, DefaultBranch: out.DefaultBranch, AllowSquashMerge: squash, Archived: out.Archived}, nil
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

// CheckSuites lists the check suites on a commit.
func (a *App) CheckSuites(ctx context.Context, repo Repo, sha string) ([]CheckSuite, error) {
	var all []CheckSuite
	for page := 1; page <= listMaxPages; page++ {
		var got struct {
			Suites []struct {
				ID         int64   `json:"id"`
				HeadSHA    string  `json:"head_sha"`
				Status     *string `json:"status"`
				Conclusion *string `json:"conclusion"`
				App        *struct {
					ID   int64  `json:"id"`
					Slug string `json:"slug"`
				} `json:"app"`
			} `json:"check_suites"`
		}
		path := repoPath(repo, "commits", sha, "check-suites") + fmt.Sprintf("?per_page=%d&page=%d", listPerPage, page)
		if err := a.call(ctx, repo, Permissions{"checks": Read}, http.MethodGet, path, nil, &got); err != nil {
			return nil, err
		}
		for _, s := range got.Suites {
			cs := CheckSuite{ID: s.ID, HeadSHA: s.HeadSHA, Status: deref(s.Status), Conclusion: deref(s.Conclusion)}
			if s.App != nil {
				cs.AppID, cs.AppSlug = s.App.ID, s.App.Slug
			}
			all = append(all, cs)
		}
		if len(got.Suites) < listPerPage {
			return all, nil
		}
	}
	return nil, ErrIncomplete
}

// CheckRuns lists the check runs on a commit, including earlier runs of the same check.
func (a *App) CheckRuns(ctx context.Context, repo Repo, sha string) ([]CheckRun, error) {
	var all []CheckRun
	for page := 1; page <= listMaxPages; page++ {
		var got struct {
			Runs []struct {
				ID         int64   `json:"id"`
				Name       string  `json:"name"`
				HeadSHA    string  `json:"head_sha"`
				Status     *string `json:"status"`
				Conclusion *string `json:"conclusion"`
				App        *struct {
					ID int64 `json:"id"`
				} `json:"app"`
			} `json:"check_runs"`
		}
		path := repoPath(repo, "commits", sha, "check-runs") + fmt.Sprintf("?per_page=%d&page=%d&filter=all", listPerPage, page)
		if err := a.call(ctx, repo, Permissions{"checks": Read}, http.MethodGet, path, nil, &got); err != nil {
			return nil, err
		}
		for _, r := range got.Runs {
			cr := CheckRun{ID: r.ID, Name: r.Name, HeadSHA: r.HeadSHA, Status: deref(r.Status), Conclusion: deref(r.Conclusion)}
			if r.App != nil {
				cr.AppID = r.App.ID
			}
			all = append(all, cr)
		}
		if len(got.Runs) < listPerPage {
			return all, nil
		}
	}
	return nil, ErrIncomplete
}

// CommitStatuses lists the commit statuses on a commit, newest first.
func (a *App) CommitStatuses(ctx context.Context, repo Repo, sha string) ([]CommitStatus, error) {
	var all []CommitStatus
	for page := 1; page <= listMaxPages; page++ {
		var got []struct {
			ID      int64  `json:"id"`
			Context string `json:"context"`
			State   string `json:"state"`
		}
		path := repoPath(repo, "commits", sha, "statuses") + fmt.Sprintf("?per_page=%d&page=%d", listPerPage, page)
		if err := a.call(ctx, repo, Permissions{"statuses": Read}, http.MethodGet, path, nil, &got); err != nil {
			return nil, err
		}
		for _, s := range got {
			all = append(all, CommitStatus{ID: s.ID, Context: s.Context, State: s.State})
		}
		if len(got) < listPerPage {
			return all, nil
		}
	}
	return nil, ErrIncomplete
}

// BranchProtection reads classic branch protection. A branch with none is Present false, not an
// error. Any other failure, 403 included, comes back as the status error for the caller to judge:
// whether a 403 means "not available on this plan" is the merge package's decision, made from the
// status and the endpoint alone.
func (a *App) BranchProtection(ctx context.Context, repo Repo, branch string) (*Protection, error) {
	p, err := branchPath(branch)
	if err != nil {
		return nil, err
	}
	var out struct {
		RequiredStatusChecks *struct {
			Contexts []string `json:"contexts"`
			Checks   []struct {
				Context string `json:"context"`
			} `json:"checks"`
		} `json:"required_status_checks"`
	}
	err = a.call(ctx, repo, Permissions{"administration": Read}, http.MethodGet, repoPath(repo, "branches", p, "protection"), nil, &out)
	if errors.Is(err, ErrNotFound) {
		return &Protection{}, nil
	}
	if err != nil {
		return nil, err
	}
	prot := &Protection{Present: true}
	if rsc := out.RequiredStatusChecks; rsc != nil {
		prot.RequiredChecks = append(prot.RequiredChecks, rsc.Contexts...)
		for _, c := range rsc.Checks {
			prot.RequiredChecks = append(prot.RequiredChecks, c.Context)
		}
	}
	return prot, nil
}

// BranchRules reads the active ruleset rules that apply to a branch.
func (a *App) BranchRules(ctx context.Context, repo Repo, branch string) ([]BranchRule, error) {
	p, err := branchPath(branch)
	if err != nil || strings.Contains(p, "/") {
		return nil, ErrBranchName
	}
	var all []BranchRule
	for page := 1; page <= listMaxPages; page++ {
		var got []struct {
			Type       string `json:"type"`
			Parameters *struct {
				RequiredStatusChecks []struct {
					Context string `json:"context"`
				} `json:"required_status_checks"`
			} `json:"parameters"`
		}
		path := repoPath(repo, "rules", "branches", p) + fmt.Sprintf("?per_page=%d&page=%d", listPerPage, page)
		if err := a.call(ctx, repo, Permissions{"metadata": Read}, http.MethodGet, path, nil, &got); err != nil {
			return nil, err
		}
		for _, r := range got {
			rule := BranchRule{Type: r.Type}
			if r.Parameters != nil {
				for _, c := range r.Parameters.RequiredStatusChecks {
					rule.RequiredChecks = append(rule.RequiredChecks, c.Context)
				}
			}
			all = append(all, rule)
		}
		if len(got) < listPerPage {
			return all, nil
		}
	}
	return nil, ErrIncomplete
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
