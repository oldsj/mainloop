// Package dotfiles holds the agent dotfiles source (docs/specs/agent-dotfiles.md): the GitHub
// repository, ref and directory whose AGENTS.md, skills, subagents and commands agent sessions
// receive as native user-level files. This slice stores the owner-wide and per-project sources.
package dotfiles

import (
	"errors"
	"strings"

	"github.com/oldsj/mainloop/server/internal/gitref"
)

// MaxPathLen is the longest directory accepted, as the contract and the table check.
const MaxPathLen = 255

// Source is one stored dotfiles source. An empty Repository is "none set".
type Source struct {
	Repository string
	Ref        string
	Path       string
}

// Normalize checks a source the contract's schema already accepted, and returns it in stored
// form: with no repository, ref and path are dropped; a trailing slash on path is dropped.
func Normalize(s Source) (Source, error) {
	if s.Repository == "" {
		return Source{}, nil
	}
	if s.Ref != "" && !gitref.ValidCheckoutRef(s.Ref) {
		return Source{}, errors.New("ref is not a valid branch, tag or commit")
	}
	path, err := cleanPath(s.Path)
	if err != nil {
		return Source{}, err
	}
	s.Path = path
	return s, nil
}

// cleanPath accepts a relative directory made of plain segments: no leading slash, no empty, "."
// or ".." segment, no backslash or control character. One trailing slash is dropped.
func cleanPath(p string) (string, error) {
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "", nil
	}
	if len(p) > MaxPathLen {
		return "", errors.New("path is too long")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return "", errors.New("path must be a directory relative to the repository root")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("path holds a control character")
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", errors.New(`path must not hold empty, "." or ".." segments`)
		}
	}
	return p, nil
}
