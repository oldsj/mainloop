package githubtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// File is one changed file of a pull request.
type File struct {
	Filename         string
	PreviousFilename string
	Status           string
	Additions        int
	Deletions        int
}

// CheckSuite is a check suite on a commit. Status defaults to "completed". HeadSHA defaults to
// the commit it is listed under. CreatedAt is an RFC 3339 time; RunsCount, when set, is the
// suite's latest_check_runs_count.
type CheckSuite struct {
	ID         int64
	AppID      int64
	Status     string
	Conclusion string
	HeadSHA    string
	CreatedAt  string
	RunsCount  *int
}

// CheckRun is a check run on a commit. Status defaults to "completed"; SuiteID to 1; HeadSHA to the
// commit it is listed under.
type CheckRun struct {
	ID         int64
	AppID      int64
	SuiteID    int64
	Name       string
	Status     string
	Conclusion string
	HeadSHA    string
}

// Status is a commit status. CreatedAt defaults to a time that grows with ID.
type Status struct {
	ID        int64
	Context   string
	State     string
	CreatedAt time.Time
}

// AppCheck is a required status check bound to an app (0 or -1: any app).
type AppCheck struct {
	Context string
	AppID   int64
}

// Protection is classic branch protection. RequiredChecks are contexts with no app; Checks are
// bound to an app. Strict requires the head to contain the latest base.
type Protection struct {
	RequiredChecks []string
	Checks         []AppCheck
	Strict         bool
}

// Rule is an active ruleset rule on the default branch. RequiredChecks and Checks make a
// required_status_checks rule; Parameters, when set, is the JSON parameters object instead.
type Rule struct {
	Type           string
	RequiredChecks []string
	Checks         []AppCheck
	Strict         bool
	Parameters     string
}

// CompareCommit is a commit of a comparison with its parents.
type CompareCommit struct {
	SHA     string
	Parents []string
}

// Compare is the answer to a commit comparison. NoFiles omits the file list, as GitHub does after
// the first page. Commits are listed page by page, 100 at a time.
type Compare struct {
	Status    string
	AheadBy   int
	BehindBy  int
	Base      string
	MergeBase string
	Commits   []CompareCommit
	Files     []File
	NoFiles   bool
}

// Update runs fn on the stored repository while holding the fake's lock, so a test can change
// what GitHub will answer next: move a branch, push a new head, close a pull request.
func (s *Server) Update(owner, name string, fn func(*Repository)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.repos[lower(owner, name)]; r != nil {
		fn(r)
	}
}

// PullRequestOf returns a copy of one pull request as the fake holds it, and whether it exists.
func (s *Server) PullRequestOf(owner, name string, number int) (PullRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.repos[lower(owner, name)]; r != nil {
		for _, p := range r.PullRequests {
			if p.Number == number {
				return p, true
			}
		}
	}
	return PullRequest{}, false
}

// RateLimit makes method and path answer the way GitHub does when the rate limit is spent: a 403
// with the remaining count at zero.
func (s *Server) RateLimit(method, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimited[method+" "+path] = true
}

// ClearRateLimit undoes [Server.RateLimit].
func (s *Server) ClearRateLimit(method, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rateLimited, method+" "+path)
}

// LoseReply makes the next n requests for method and path take effect and then lose their answer:
// the connection is closed before any response is written. It is how a test makes a create or a
// merge whose outcome the client cannot know.
func (s *Server) LoseReply(method, path string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lost[method+" "+path] = n
}

// Count returns how many requests for method and path were received.
func (s *Server) Count(method, path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

// fakeSHA makes a 40-hex commit id from a seed, for commits the fake itself creates.
func fakeSHA(seed string) string {
	var sum uint64 = 1469598103934665603
	out := strings.Builder{}
	for i := 0; out.Len() < 40; i++ {
		for _, c := range seed + strconv.Itoa(i) {
			sum ^= uint64(c) //nolint:gosec // a hash of ASCII text
			sum *= 1099511628211
		}
		fmt.Fprintf(&out, "%016x", sum)
	}
	return out.String()[:40]
}

func openCount(repo *Repository) int {
	n := 0
	for _, p := range repo.PullRequests {
		if !p.Closed && !p.Merged {
			n++
		}
	}
	return n
}

func repoID(repo *Repository) int64 {
	if repo.ID == 0 {
		return 1
	}
	return repo.ID
}

func baseRef(repo *Repository, p PullRequest) string {
	if p.BaseRef != "" {
		return p.BaseRef
	}
	return repo.DefaultBranch
}

// TipOf is the commit a branch of the repository points at: the fake's own for the default branch
// unless a test set one. It is empty for an unknown branch.
func TipOf(repo *Repository, branch string) string {
	sha, _ := tipOf(repo, branch)
	return sha
}

func tipOf(repo *Repository, branch string) (string, bool) {
	if sha, ok := repo.Branches[branch]; ok {
		return sha, true
	}
	if branch == repo.DefaultBranch {
		return fakeSHA("base:" + repo.Owner + "/" + repo.Name), true
	}
	return "", false
}

func prState(p PullRequest) string {
	if p.Closed || p.Merged {
		return "closed"
	}
	return "open"
}

func refJSON(repo *Repository, ref, sha string, gone bool) map[string]any {
	out := map[string]any{"ref": ref, "sha": sha}
	if gone {
		out["repo"] = nil
	} else {
		out["repo"] = map[string]any{"id": repoID(repo), "full_name": repo.Owner + "/" + repo.Name}
	}
	return out
}

func pullJSON(repo *Repository, p PullRequest, detail bool) map[string]any {
	base, _ := tipOf(repo, baseRef(repo, p))
	out := map[string]any{
		"number": p.Number, "title": p.Title, "state": prState(p), "draft": p.Draft,
		"user":       map[string]any{"login": p.Author},
		"created_at": p.CreatedAt.UTC().Format(time.RFC3339), "updated_at": p.UpdatedAt.UTC().Format(time.RFC3339),
		"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", repo.Owner, repo.Name, p.Number),
		"head":     refJSON(repo, p.HeadRef, p.HeadSHA, p.HeadRepoGone),
		"base":     refJSON(repo, baseRef(repo, p), base, false),
	}
	if !detail {
		return out
	}
	adds, dels := p.Additions, p.Deletions
	changed := p.ChangedFiles
	if adds == 0 && dels == 0 {
		for _, f := range p.Files {
			adds += f.Additions
			dels += f.Deletions
		}
	}
	if changed == 0 {
		changed = len(p.Files)
	}
	state := p.MergeableState
	if state == "" {
		state = "clean"
	}
	out["body"] = p.Body
	out["merged"] = p.Merged
	out["additions"], out["deletions"], out["changed_files"] = adds, dels, changed
	switch {
	case p.MergeableUnknown:
		out["mergeable"], out["mergeable_state"] = nil, "unknown"
	case p.Unmergeable:
		out["mergeable"], out["mergeable_state"] = false, "dirty"
	default:
		out["mergeable"], out["mergeable_state"] = true, state
	}
	if p.MergeCommitSHA != "" {
		out["merge_commit_sha"] = p.MergeCommitSHA
	}
	return out
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func (s *Server) pullCall(w http.ResponseWriter, r *http.Request, repo *Repository, rest []string) {
	switch {
	case len(rest) == 1 && r.Method == http.MethodGet:
		s.pullList(w, r, repo)
	case len(rest) == 1 && r.Method == http.MethodPost:
		s.pullCreate(w, r, repo)
	case len(rest) == 2:
		s.pullGet(w, repo, rest[1])
	case len(rest) == 3 && rest[2] == "files":
		s.pullFiles(w, r, repo, rest[1])
	case len(rest) == 3 && rest[2] == "merge":
		s.pullMerge(w, r, repo, rest[1])
	default:
		notFound(w)
	}
}

func (s *Server) pullList(w http.ResponseWriter, r *http.Request, repo *Repository) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	head := strings.TrimPrefix(q.Get("head"), repo.Owner+":")
	s.mu.Lock()
	var all []PullRequest
	for _, p := range repo.PullRequests {
		if state != "all" && prState(p) != state {
			continue
		}
		if q.Get("head") != "" && p.HeadRef != head {
			continue
		}
		if b := q.Get("base"); b != "" && baseRef(repo, p) != b {
			continue
		}
		all = append(all, p)
	}
	from, to, pageNo, last := page(r, len(all))
	out := []map[string]any{}
	for _, p := range all[from:to] {
		out = append(out, pullJSON(repo, p, false))
	}
	s.mu.Unlock()
	s.link(w, r, pageNo, last)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) pullCreate(w http.ResponseWriter, r *http.Request, repo *Repository) {
	var in struct {
		Head  string `json:"head"`
		Base  string `json:"base"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Head == "" || in.Base == "" || in.Title == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sha, ok := tipOf(repo, in.Head)
	if _, baseOK := tipOf(repo, in.Base); !ok || !baseOK {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
		return
	}
	for _, p := range repo.PullRequests {
		if p.HeadRef == in.Head && baseRef(repo, p) == in.Base && prState(p) == "open" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "A pull request already exists"})
			return
		}
	}
	key := lower(repo.Owner, repo.Name)
	n := s.nextPR[key]
	for _, p := range repo.PullRequests {
		n = max(n, p.Number)
	}
	n++
	s.nextPR[key] = n
	now := time.Now().UTC()
	p := PullRequest{
		Number: n, Title: in.Title, Body: in.Body, Author: "mainloop[bot]", HeadRef: in.Head, BaseRef: in.Base,
		HeadSHA: sha, CreatedAt: now, UpdatedAt: now,
	}
	repo.PullRequests = append(repo.PullRequests, p)
	writeJSON(w, http.StatusCreated, pullJSON(repo, p, true))
}

func (s *Server) find(repo *Repository, num string) (int, bool) {
	n, err := strconv.Atoi(num)
	if err != nil {
		return 0, false
	}
	for i, p := range repo.PullRequests {
		if p.Number == n {
			return i, true
		}
	}
	return 0, false
}

func (s *Server) pullGet(w http.ResponseWriter, repo *Repository, num string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.find(repo, num)
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, pullJSON(repo, repo.PullRequests[i], true))
}

func (s *Server) pullFiles(w http.ResponseWriter, r *http.Request, repo *Repository, num string) {
	s.mu.Lock()
	i, ok := s.find(repo, num)
	if !ok {
		s.mu.Unlock()
		notFound(w)
		return
	}
	files := append([]File(nil), repo.PullRequests[i].Files...)
	s.mu.Unlock()
	from, to, pageNo, last := page(r, len(files))
	out := []map[string]any{}
	for _, f := range files[from:to] {
		status := f.Status
		if status == "" {
			status = "modified"
		}
		item := map[string]any{"filename": f.Filename, "status": status, "additions": f.Additions, "deletions": f.Deletions, "changes": f.Additions + f.Deletions}
		if f.PreviousFilename != "" {
			item["previous_filename"] = f.PreviousFilename
		}
		out = append(out, item)
	}
	s.link(w, r, pageNo, last)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) pullMerge(w http.ResponseWriter, r *http.Request, repo *Repository, num string) {
	var in struct {
		SHA         string `json:"sha"`
		MergeMethod string `json:"merge_method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.find(repo, num)
	if !ok {
		notFound(w)
		return
	}
	p := &repo.PullRequests[i]
	switch {
	case repo.MergeStatus != 0:
		writeJSON(w, repo.MergeStatus, map[string]string{"message": SecretMarker})
	case prState(*p) != "open":
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Pull Request is not mergeable"})
	case in.SHA != p.HeadSHA:
		writeJSON(w, http.StatusConflict, map[string]string{"message": "Head branch was modified. Review and try the merge again."})
	case in.MergeMethod == "squash" && repo.NoSquash:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Squash merges are not allowed on this repository."})
	default:
		p.Merged, p.Closed = true, true
		p.MergeCommitSHA = fakeSHA(fmt.Sprintf("merge:%s/%s:%d", repo.Owner, repo.Name, p.Number))
		if repo.Branches == nil {
			repo.Branches = map[string]string{}
		}
		repo.Branches[baseRef(repo, *p)] = p.MergeCommitSHA
		writeJSON(w, http.StatusOK, map[string]any{"sha": p.MergeCommitSHA, "merged": true, "message": "Pull Request successfully merged"})
	}
}

func (s *Server) branchCall(w http.ResponseWriter, r *http.Request, repo *Repository, rest []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(rest); n >= 2 && rest[n-1] == "protection" {
		if repo.ProtectionStatus != 0 {
			writeJSON(w, repo.ProtectionStatus, map[string]string{"message": messageOr(repo.ProtectionMessage)})
			return
		}
		if repo.ProtectionRaw != "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(repo.ProtectionRaw))
			return
		}
		if repo.Protection == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Branch not protected"})
			return
		}
		checks := []map[string]any{}
		for _, c := range repo.Protection.Checks {
			checks = append(checks, map[string]any{"context": c.Context, "app_id": c.AppID})
		}
		contexts := append([]string{}, repo.Protection.RequiredChecks...)
		out := map[string]any{"required_status_checks": nil}
		if len(checks) > 0 || len(contexts) > 0 || repo.Protection.Strict {
			out["required_status_checks"] = map[string]any{"strict": repo.Protection.Strict, "contexts": contexts, "checks": checks}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	branch := strings.Join(rest, "/")
	sha, ok := tipOf(repo, branch)
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": branch, "commit": map[string]any{"sha": sha}})
}

func messageOr(m string) string {
	if m == "" {
		return SecretMarker
	}
	return m
}

func (s *Server) ruleCall(w http.ResponseWriter, r *http.Request, repo *Repository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if repo.RulesStatus != 0 {
		writeJSON(w, repo.RulesStatus, map[string]string{"message": messageOr(repo.RulesMessage)})
		return
	}
	if p, _ := strconv.Atoi(r.URL.Query().Get("page")); p > 1 && repo.RulesLaterStatus != 0 {
		writeJSON(w, repo.RulesLaterStatus, map[string]string{"message": messageOr(repo.RulesMessage)})
		return
	}
	if repo.RulesRaw != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(repo.RulesRaw))
		return
	}
	out := []json.RawMessage{}
	for _, rule := range repo.Rules {
		item := map[string]any{"type": rule.Type, "ruleset_id": 1}
		switch {
		case rule.Parameters != "":
			item["parameters"] = json.RawMessage(rule.Parameters)
		case len(rule.RequiredChecks) > 0 || len(rule.Checks) > 0 || rule.Strict:
			checks := []map[string]any{}
			for _, c := range rule.RequiredChecks {
				checks = append(checks, map[string]any{"context": c})
			}
			for _, c := range rule.Checks {
				checks = append(checks, map[string]any{"context": c.Context, "integration_id": c.AppID})
			}
			item["parameters"] = map[string]any{"required_status_checks": checks, "strict_required_status_checks_policy": rule.Strict}
		}
		raw, _ := json.Marshal(item)
		out = append(out, raw)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) compareCall(w http.ResponseWriter, r *http.Request, repo *Repository, spec string) {
	base, head, ok := strings.Cut(spec, "...")
	if !ok {
		notFound(w)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, found := repo.Compares[spec]
	if !found {
		tip, _ := tipOf(repo, repo.DefaultBranch)
		if base != tip {
			notFound(w)
			return
		}
		for _, p := range repo.PullRequests {
			if p.HeadSHA == head {
				c = Compare{Status: "ahead", AheadBy: 1, Base: base, MergeBase: base, Commits: []CompareCommit{{SHA: head, Parents: []string{base}}}, Files: p.Files}
				found = true
			}
		}
		if !found {
			notFound(w)
			return
		}
	}
	if c.Base == "" {
		c.Base = base
	}
	if c.MergeBase == "" {
		c.MergeBase = base
	}
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if per < 1 || per > 100 {
		per = 30
	}
	pg = max(pg, 1)
	from, to := min((pg-1)*per, len(c.Commits)), min(pg*per, len(c.Commits))
	commits := []map[string]any{}
	for _, x := range c.Commits[from:to] {
		parents := []map[string]any{}
		for _, p := range x.Parents {
			parents = append(parents, map[string]any{"sha": p})
		}
		commits = append(commits, map[string]any{"sha": x.SHA, "parents": parents})
	}
	out := map[string]any{
		"status": c.Status, "ahead_by": c.AheadBy, "behind_by": c.BehindBy,
		"base_commit": map[string]any{"sha": c.Base}, "merge_base_commit": map[string]any{"sha": c.MergeBase},
		"commits": commits,
	}
	if pg == 1 && !c.NoFiles {
		files := []map[string]any{}
		for _, f := range c.Files {
			status := f.Status
			if status == "" {
				status = "modified"
			}
			item := map[string]any{"filename": f.Filename, "status": status, "additions": f.Additions, "deletions": f.Deletions}
			if f.PreviousFilename != "" {
				item["previous_filename"] = f.PreviousFilename
			}
			files = append(files, item)
		}
		out["files"] = files
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) commitFacts(w http.ResponseWriter, r *http.Request, repo *Repository, sha, kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case "check-suites":
		suites := []map[string]any{}
		for _, c := range repo.Suites[sha] {
			status := c.Status
			if status == "" {
				status = "completed"
			}
			var conclusion any
			if c.Conclusion != "" {
				conclusion = c.Conclusion
			}
			head := c.HeadSHA
			if head == "" {
				head = sha
			}
			item := map[string]any{"id": c.ID, "head_sha": head, "status": status, "conclusion": conclusion, "app": map[string]any{"id": c.AppID, "slug": "app"}}
			if c.CreatedAt != "" {
				item["created_at"] = c.CreatedAt
			}
			if c.RunsCount != nil {
				item["latest_check_runs_count"] = *c.RunsCount
			}
			suites = append(suites, item)
		}
		from, to, pageNo, last := page(r, len(suites))
		s.link(w, r, pageNo, last)
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(suites), "check_suites": suites[from:to]})
	case "check-runs":
		runs := []map[string]any{}
		for _, c := range repo.Runs[sha] {
			status := c.Status
			if status == "" {
				status = "completed"
			}
			var conclusion any
			if c.Conclusion != "" {
				conclusion = c.Conclusion
			}
			head := c.HeadSHA
			if head == "" {
				head = sha
			}
			suite := c.SuiteID
			if suite == 0 {
				suite = 1
			}
			runs = append(runs, map[string]any{"id": c.ID, "name": c.Name, "head_sha": head, "status": status, "conclusion": conclusion, "app": map[string]any{"id": c.AppID}, "check_suite": map[string]any{"id": suite}})
		}
		from, to, pageNo, last := page(r, len(runs))
		s.link(w, r, pageNo, last)
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "check_runs": runs[from:to]})
	default:
		statuses := []map[string]any{}
		for _, c := range repo.Statuses[sha] {
			created := c.CreatedAt
			if created.IsZero() {
				created = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(c.ID) * time.Second)
			}
			statuses = append(statuses, map[string]any{"id": c.ID, "context": c.Context, "state": c.State, "created_at": created.UTC().Format(time.RFC3339)})
		}
		from, to, pageNo, last := page(r, len(statuses))
		s.link(w, r, pageNo, last)
		writeJSON(w, http.StatusOK, statuses[from:to])
	}
}
