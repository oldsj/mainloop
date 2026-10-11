// Package github is Mainloop's only route to the GitHub API: GitHub App authentication, a typed
// client for the few calls the control plane makes, and the endpoint allowlist every request
// passes through. Installation tokens never leave this package.
package github

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// ErrInvalidRepo means the text is not a GitHub repository reference Mainloop accepts.
var ErrInvalidRepo = errors.New("invalid GitHub repository")

var (
	// A GitHub login: alphanumerics and single hyphens inside, at most 39 characters.
	ownerRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	// A repository name: alphanumerics, '.', '_' and '-', at most 100 characters.
	nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// Repo is a GitHub repository identity. Case is kept for display; GitHub names are
// case-insensitive, so compare with [Repo.Same].
type Repo struct {
	Owner string
	Name  string
}

// FullName is "owner/name".
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// HTMLURL is the repository's web address.
func (r Repo) HTMLURL() string { return "https://github.com/" + r.FullName() }

// Same reports whether two repositories are the same one, ignoring case.
func (r Repo) Same(o Repo) bool {
	return strings.EqualFold(r.Owner, o.Owner) && strings.EqualFold(r.Name, o.Name)
}

// ParseRepo returns the repository named by "owner/name" or "https://github.com/owner/name[.git]".
// It is strict on purpose: only github.com over https, exactly an owner and a name with at most
// one trailing slash, and no credentials, port, query or fragment.
func ParseRepo(value string) (Repo, error) {
	text := strings.TrimSpace(value)
	if text == "" {
		return Repo{}, ErrInvalidRepo
	}
	if strings.Contains(text, "://") {
		path, ok := githubURLPath(text)
		if !ok {
			return Repo{}, ErrInvalidRepo
		}
		text = path
	}
	parts := strings.Split(strings.TrimSuffix(text, "/"), "/")
	if len(parts) != 2 {
		return Repo{}, ErrInvalidRepo
	}
	owner, name := parts[0], parts[1]
	if n := len(name); n >= 4 && strings.EqualFold(name[n-4:], ".git") {
		name = name[:n-4]
	}
	if !ownerRE.MatchString(owner) || !nameRE.MatchString(name) || name == "." || name == ".." {
		return Repo{}, ErrInvalidRepo
	}
	return Repo{Owner: owner, Name: name}, nil
}

func githubURLPath(text string) (string, bool) {
	// A query or fragment is refused wherever it sits, including an empty one.
	if strings.ContainsAny(text, "?#") {
		return "", false
	}
	u, err := url.Parse(text)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "github.com") {
		return "", false
	}
	if u.User != nil || u.Port() != "" || u.Host != u.Hostname() {
		return "", false
	}
	return strings.TrimPrefix(u.Path, "/"), true
}
