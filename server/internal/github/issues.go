package github

import (
	"context"
	"net/http"
	"strconv"
)

// Issue is one GitHub issue as the read-only issue tool shows it.
type Issue struct {
	Number int
	Title  string
	Body   string
	// BodyAvailable distinguishes an observed empty body from null or missing text.
	BodyAvailable bool
	State         string
	Labels        []string
	HTMLURL       string
}

type issueJSON struct {
	Number int         `json:"number"`
	Title  string      `json:"title"`
	Body   *pullBody   `json:"body"`
	State  string      `json:"state"`
	Labels []labelJSON `json:"labels"`
	// PullRequest is set only when the issue number actually names a pull request: GitHub serves
	// both from the same endpoint.
	PullRequest *struct{} `json:"pull_request"`
	HTMLURL     string    `json:"html_url"`
}

// labelJSON accepts the one shape the REST API sends for a non-empty name: an object. A bare
// string label name was retired long ago and is not decoded.
type labelJSON struct {
	Name string `json:"name"`
}

// GetIssue reads one issue. A number that names a pull request, not an issue, is [ErrNotFound]:
// the two share a numbering space and an endpoint, but this reader serves issues only.
func (a *App) GetIssue(ctx context.Context, repo Repo, number int) (*Issue, error) {
	var out issueJSON
	if err := a.call(ctx, repo, Permissions{"issues": Read}, http.MethodGet, repoPath(repo, "issues", strconv.Itoa(number)), nil, &out); err != nil {
		return nil, err
	}
	if out.Number != number || out.PullRequest != nil {
		return nil, ErrNotFound
	}
	labels := make([]string, 0, len(out.Labels))
	for _, l := range out.Labels {
		labels = append(labels, l.Name)
	}
	issue := &Issue{Number: out.Number, Title: out.Title, State: out.State, Labels: labels, HTMLURL: out.HTMLURL}
	if out.Body != nil {
		issue.Body, issue.BodyAvailable = string(*out.Body), true
	}
	return issue, nil
}
