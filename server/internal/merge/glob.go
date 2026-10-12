package merge

import (
	"errors"
	"slices"
	"strings"
)

// ProtectedGlobs are the fixed path globs that always need the owner's approval, and
// GlobsVersion is their version. They are not configurable.
var ProtectedGlobs = []string{"k8s/**", ".github/**", "**/migrations/**"}

// GlobsVersion is the version of ProtectedGlobs.
const GlobsVersion = 1

// MaxFiles is the most changed files a preparation reads; a larger pull request is unavailable.
const MaxFiles = 3000

// errPath marks a path that cannot be matched safely, which makes the evidence unavailable.
var errPath = errors.New("invalid repository-relative path")

// matchedGlobs returns the protected globs a repository-relative path matches (case sensitive,
// POSIX separators).
func matchedGlobs(path string) []string {
	parts := strings.Split(path, "/")
	var out []string
	if len(parts) > 1 && parts[0] == "k8s" {
		out = append(out, "k8s/**")
	}
	if len(parts) > 1 && parts[0] == ".github" {
		out = append(out, ".github/**")
	}
	if slices.Contains(parts[:len(parts)-1], "migrations") {
		out = append(out, "**/migrations/**")
	}
	return out
}

// validPath rejects paths whose matching could be fooled: empty, dot and dot-dot parts, a
// backslash or a control character.
func validPath(name string) bool {
	if name == "" || strings.Contains(name, `\`) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	for _, r := range name {
		if r < 32 {
			return false
		}
	}
	return true
}

// ProtectedMatch is one changed path that matched a protected glob.
type ProtectedMatch struct {
	Path string `json:"path"`
	Glob string `json:"glob"`
}

// protectedMatches returns every changed path (new and previous names) that matches a protected
// glob, sorted, and an error when any path is not safe to match.
func protectedMatches(files []FileFact) ([]ProtectedMatch, error) {
	var out []ProtectedMatch
	seen := map[string]bool{}
	for _, f := range files {
		if f.Filename == "" || (f.Status == "renamed" && f.PreviousFilename == "") {
			return nil, errPath
		}
		for _, name := range []string{f.Filename, f.PreviousFilename} {
			if name == "" {
				continue
			}
			if !validPath(name) {
				return nil, errPath
			}
			for _, g := range matchedGlobs(name) {
				if k := name + "\x00" + g; !seen[k] {
					seen[k] = true
					out = append(out, ProtectedMatch{Path: name, Glob: g})
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b ProtectedMatch) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.Glob, b.Glob)
	})
	return out, nil
}
