package githubtest

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Issue is an issue in the fake. State defaults to "open". IsPullRequest makes the fake answer
// with a non-null "pull_request" field, the way GitHub does when the number names a pull request
// instead of an issue.
type Issue struct {
	Number        int
	Title         string
	Body          string
	State         string
	Labels        []string
	IsPullRequest bool
	// CreatedAt orders the issue listing (zero sorts oldest). UserType is the author's account type,
	// "User" when empty; issues the App creates are "Bot".
	CreatedAt time.Time
	UserType  string
	// Comments are the issue's conversation comments, oldest first.
	Comments []IssueComment
}

// IssueComment is a comment on an issue in the fake. UserType is "User" when empty; comments the
// App creates are "Bot".
type IssueComment struct {
	ID        int64
	Body      string
	UserType  string
	CreatedAt time.Time
}

func userType(t string) string {
	if t == "" {
		return "User"
	}
	return t
}

func issueURL(repo *Repository, n int) string {
	return "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(n)
}

// issueIndex finds an issue (not a pull request) by number. The caller holds s.mu.
func issueIndex(repo *Repository, num string) (int, bool) {
	n, err := strconv.Atoi(num)
	if err != nil {
		return 0, false
	}
	for i, is := range repo.IssueList {
		if is.Number == n && !is.IsPullRequest {
			return i, true
		}
	}
	return 0, false
}

// issueListCall serves GET /issues: issues and pull-request issues alike, sorted by creation,
// newest first by default (sort=created only), one page of up to per_page.
func (s *Server) issueListCall(w http.ResponseWriter, r *http.Request, repo *Repository) {
	s.mu.Lock()
	all := append([]Issue(nil), repo.IssueList...)
	s.mu.Unlock()
	if r.URL.Query().Get("direction") == "asc" {
		sortIssues(all, false)
	} else {
		sortIssues(all, true)
	}
	from, to, pageNo, last := page(r, len(all))
	out := []map[string]any{}
	for _, i := range all[from:to] {
		m := map[string]any{
			"number": i.Number, "title": i.Title, "body": i.Body, "state": issueState(i),
			"html_url": issueURL(repo, i.Number), "created_at": i.CreatedAt.UTC().Format(time.RFC3339),
			"user": map[string]any{"type": userType(i.UserType)}, "pull_request": nil,
		}
		if i.IsPullRequest {
			m["pull_request"] = map[string]any{}
		}
		out = append(out, m)
	}
	s.link(w, r, pageNo, last)
	writeJSON(w, http.StatusOK, out)
}

func sortIssues(list []Issue, newestFirst bool) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			a, b := list[j-1], list[j]
			swap := a.CreatedAt.After(b.CreatedAt) || (a.CreatedAt.Equal(b.CreatedAt) && a.Number > b.Number)
			if newestFirst {
				swap = b.CreatedAt.After(a.CreatedAt) || (a.CreatedAt.Equal(b.CreatedAt) && b.Number > a.Number)
			}
			if !swap {
				break
			}
			list[j-1], list[j] = b, a
		}
	}
}

// createIssueCall serves POST /issues. Every label must exist in the repository (Labels), or the
// fake answers 422; GitHub's own handling of an unknown label depends on the caller's access.
func (s *Server) createIssueCall(w http.ResponseWriter, r *http.Request, repo *Repository) {
	var in struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Title == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range in.Labels {
		found := false
		for _, have := range repo.Labels {
			found = found || have == l
		}
		if !found {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
			return
		}
	}
	key := lower(repo.Owner, repo.Name)
	n := s.nextPR[key]
	for _, p := range repo.PullRequests {
		n = max(n, p.Number)
	}
	for _, i := range repo.IssueList {
		n = max(n, i.Number)
	}
	n++
	s.nextPR[key] = n
	is := Issue{Number: n, Title: in.Title, Body: in.Body, Labels: in.Labels, CreatedAt: time.Now().UTC(), UserType: "Bot"}
	repo.IssueList = append(repo.IssueList, is)
	writeJSON(w, http.StatusCreated, map[string]any{
		"number": n, "title": is.Title, "body": is.Body, "state": "open", "html_url": issueURL(repo, n),
		"created_at": is.CreatedAt.Format(time.RFC3339), "user": map[string]any{"type": "Bot"}, "pull_request": nil,
	})
}

// issueCommentsCall serves GET /issues/{n}/comments for an issue, oldest first, honouring since.
func (s *Server) issueCommentsCall(w http.ResponseWriter, r *http.Request, repo *Repository, num string) {
	s.mu.Lock()
	i, ok := issueIndex(repo, num)
	var items []IssueComment
	if ok {
		items = append(items, repo.IssueList[i].Comments...)
	}
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
			return
		}
		since = t
	}
	out := []map[string]any{}
	for _, c := range items {
		if !since.IsZero() && c.CreatedAt.Before(since) {
			continue
		}
		out = append(out, commentJSON(repo, num, c))
	}
	from, to, pageNo, last := page(r, len(out))
	s.link(w, r, pageNo, last)
	writeJSON(w, http.StatusOK, out[from:to])
}

func commentJSON(repo *Repository, num string, c IssueComment) map[string]any {
	n, _ := strconv.Atoi(num)
	return map[string]any{
		"id": c.ID, "body": c.Body, "created_at": c.CreatedAt.UTC().Format(time.RFC3339),
		"user":     map[string]any{"type": userType(c.UserType)},
		"html_url": issueURL(repo, n) + "#issuecomment-" + strconv.FormatInt(c.ID, 10),
	}
}

// createCommentCall serves POST /issues/{n}/comments on an issue or, as GitHub does through the
// same endpoint, on a pull request.
func (s *Server) createCommentCall(w http.ResponseWriter, r *http.Request, repo *Repository, num string) {
	var in struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Body) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := issueIndex(repo, num)
	if !ok {
		s.pullCommentLocked(w, repo, num, in.Body)
		return
	}
	var id int64 = 1000
	for _, is := range repo.IssueList {
		for _, c := range is.Comments {
			id = max(id, c.ID)
		}
	}
	c := IssueComment{ID: id + 1, Body: in.Body, UserType: "Bot", CreatedAt: time.Now().UTC()}
	repo.IssueList[i].Comments = append(repo.IssueList[i].Comments, c)
	writeJSON(w, http.StatusCreated, commentJSON(repo, num, c))
}

// labelsCall serves GET /labels.
func (s *Server) labelsCall(w http.ResponseWriter, r *http.Request, repo *Repository) {
	s.mu.Lock()
	names := append([]string(nil), repo.Labels...)
	s.mu.Unlock()
	from, to, pageNo, last := page(r, len(names))
	out := []map[string]any{}
	for _, n := range names[from:to] {
		out = append(out, map[string]any{"name": n})
	}
	s.link(w, r, pageNo, last)
	writeJSON(w, http.StatusOK, out)
}

// DependabotAlert is a Dependabot alert in the fake. State defaults to "open".
type DependabotAlert struct {
	Number                 int
	State                  string
	PackageName            string
	Ecosystem              string
	ManifestPath           string
	Severity               string
	VulnerableVersionRange string
	PatchedVersion         string
	GHSAID                 string
	Summary                string
}

func issueState(i Issue) string {
	if i.State == "" {
		return "open"
	}
	return i.State
}

func (s *Server) issueCall(w http.ResponseWriter, repo *Repository, num string) {
	n, err := strconv.Atoi(num)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		notFound(w)
		return
	}
	for _, i := range repo.IssueList {
		if i.Number != n {
			continue
		}
		labels := []map[string]any{}
		for _, l := range i.Labels {
			labels = append(labels, map[string]any{"name": l})
		}
		out := map[string]any{
			"number": i.Number, "title": i.Title, "body": i.Body, "state": issueState(i), "labels": labels,
			"html_url":     "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + num,
			"pull_request": nil,
		}
		if i.IsPullRequest {
			out["pull_request"] = map[string]any{"url": "https://api.github.com/repos/" + repo.Owner + "/" + repo.Name + "/pulls/" + num}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	notFound(w)
}

func alertState(a DependabotAlert) string {
	if a.State == "" {
		return "open"
	}
	return a.State
}

func (s *Server) dependabotCall(w http.ResponseWriter, r *http.Request, repo *Repository) {
	s.mu.Lock()
	state := r.URL.Query().Get("state")
	var all []DependabotAlert
	for _, a := range repo.DependabotAlerts {
		if state != "" && state != "all" && alertState(a) != state {
			continue
		}
		all = append(all, a)
	}
	s.mu.Unlock()
	from, to, pageNo, last := page(r, len(all))
	out := []map[string]any{}
	for _, a := range all[from:to] {
		num := strconv.Itoa(a.Number)
		var patched any
		if a.PatchedVersion != "" {
			patched = map[string]any{"identifier": a.PatchedVersion}
		}
		out = append(out, map[string]any{
			"number": a.Number, "state": alertState(a),
			"dependency": map[string]any{
				"package":       map[string]any{"ecosystem": a.Ecosystem, "name": a.PackageName},
				"manifest_path": a.ManifestPath,
			},
			"security_advisory": map[string]any{"ghsa_id": a.GHSAID, "summary": a.Summary},
			"security_vulnerability": map[string]any{
				"severity": a.Severity, "vulnerable_version_range": a.VulnerableVersionRange, "first_patched_version": patched,
			},
			"url":      "https://api.github.com/repos/" + repo.Owner + "/" + repo.Name + "/dependabot/alerts/" + num,
			"html_url": "https://github.com/" + repo.Owner + "/" + repo.Name + "/security/dependabot/" + num,
		})
	}
	s.link(w, r, pageNo, last)
	writeJSON(w, http.StatusOK, out)
}

// pullCommentLocked adds the App's conversation comment to pull request num, or answers 404.
func (s *Server) pullCommentLocked(w http.ResponseWriter, repo *Repository, num, body string) {
	i, ok := s.find(repo, num)
	if !ok {
		notFound(w)
		return
	}
	p := &repo.PullRequests[i]
	var id int64 = 3000
	for _, f := range p.Feedback {
		id = max(id, f.ID)
	}
	c := Feedback{ID: id + 1, Kind: FeedbackComment, Author: "mainloop[bot]", AuthorType: "Bot", Body: body, At: time.Now().UTC()}
	p.Feedback = append(p.Feedback, c)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": c.ID, "body": body, "created_at": c.At.Format(time.RFC3339),
		"user":     map[string]any{"login": c.Author, "type": "Bot"},
		"html_url": "https://github.com/" + repo.Owner + "/" + repo.Name + "/pull/" + num + "#issuecomment-" + strconv.FormatInt(c.ID, 10),
	})
}
