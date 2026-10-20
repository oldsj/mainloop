package githubtest

import (
	"net/http"
	"strconv"
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
