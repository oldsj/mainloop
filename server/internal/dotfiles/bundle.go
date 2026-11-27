package dotfiles

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// Limits bound what one export may hold. They are checked against the allowed files only.
type Limits struct {
	MaxFiles      int
	MaxFileBytes  int64
	MaxTotalBytes int64
}

// DefaultLimits are the spec's: at most 4000 files, 2 MiB per file, 16 MiB in all.
var DefaultLimits = Limits{MaxFiles: 4000, MaxFileBytes: 2 << 20, MaxTotalBytes: 16 << 20}

// File is one exported file: its path relative to the source directory, whether git marked it
// executable, and its content.
type File struct {
	Path       string
	Executable bool
	Data       []byte
}

// Refusal is why a directory cannot be exported: the session gets no dotfiles and records Reason.
type Refusal struct {
	Reason string
	Detail string
}

func (r *Refusal) Error() string { return "dotfiles: " + r.Reason + ": " + r.Detail }

// Reasons an export is refused.
const (
	ReasonOverLimits      = "over_limits"
	ReasonUnsupportedFile = "unsupported_file"
	ReasonNothingToExport = "nothing_to_export"
	ReasonSecretFound     = "secret_found"
	ReasonInvalidSettings = "invalid_settings"
)

// Select reads a tar of the repository at one commit, as `git archive` writes it, and returns the
// files under dir that agents receive, sorted by path:
//
//   - AGENTS.md and CLAUDE.md;
//   - skills/<name>/** for each skill directory that holds a SKILL.md;
//   - agents/<name>.md and commands/<name>.md;
//   - claude/settings.json, which must pass CheckClaudeSettings.
//
// Anything else is ignored. A symlink or any other non-regular entry where a file would be kept,
// and an export past lim, are a *Refusal; nothing is partially exported.
func Select(archive io.Reader, dir string, lim Limits) ([]File, error) {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	tr := tar.NewReader(archive)
	var kept []File
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("dotfiles: read archive: %w", err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader || h.Typeflag == tar.TypeDir {
			continue
		}
		rel, ok := strings.CutPrefix(h.Name, prefix)
		if !ok || !candidate(rel) {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return nil, &Refusal{Reason: ReasonUnsupportedFile, Detail: rel + " is not a regular file"}
		}
		if !cleanRel(rel) {
			return nil, &Refusal{Reason: ReasonUnsupportedFile, Detail: "a path holds an unsupported name"}
		}
		if h.Size > lim.MaxFileBytes {
			return nil, &Refusal{Reason: ReasonOverLimits, Detail: fmt.Sprintf("%s is over %d bytes", rel, lim.MaxFileBytes)}
		}
		if total += h.Size; total > lim.MaxTotalBytes {
			return nil, &Refusal{Reason: ReasonOverLimits, Detail: fmt.Sprintf("the files are over %d bytes", lim.MaxTotalBytes)}
		}
		if len(kept) == lim.MaxFiles {
			return nil, &Refusal{Reason: ReasonOverLimits, Detail: fmt.Sprintf("more than %d files", lim.MaxFiles)}
		}
		data, err := io.ReadAll(io.LimitReader(tr, lim.MaxFileBytes+1))
		if err != nil {
			return nil, fmt.Errorf("dotfiles: read %s: %w", rel, err)
		}
		kept = append(kept, File{Path: rel, Executable: h.Mode&0o111 != 0, Data: data})
	}
	kept = withSkillManifests(kept)
	if len(kept) == 0 {
		return nil, &Refusal{Reason: ReasonNothingToExport, Detail: "the directory holds no AGENTS.md, CLAUDE.md, skills, agents, commands or claude/settings.json"}
	}
	for _, f := range kept {
		if f.Path == ClaudeSettingsPath {
			if err := CheckClaudeSettings(f.Data); err != nil {
				return nil, &Refusal{Reason: ReasonInvalidSettings, Detail: err.Error()}
			}
		}
	}
	slices.SortFunc(kept, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return kept, nil
}

// candidate reports whether rel is a path that would be exported, before a skill is checked for
// its SKILL.md.
func candidate(rel string) bool {
	switch rel {
	case "AGENTS.md", "CLAUDE.md", ClaudeSettingsPath:
		return true
	}
	parts := strings.Split(rel, "/")
	switch parts[0] {
	case "skills":
		return len(parts) >= 3
	case "agents", "commands":
		return len(parts) == 2 && strings.HasSuffix(parts[1], ".md") && parts[1] != ".md"
	}
	return false
}

// withSkillManifests drops the files of any skill directory that has no SKILL.md at its root.
func withSkillManifests(files []File) []File {
	skills := map[string]bool{}
	for _, f := range files {
		if name, rest, ok := skillPath(f.Path); ok && rest == "SKILL.md" {
			skills[name] = true
		}
	}
	return slices.DeleteFunc(files, func(f File) bool {
		name, _, ok := skillPath(f.Path)
		return ok && !skills[name]
	})
}

func skillPath(rel string) (name, rest string, ok bool) {
	after, ok := strings.CutPrefix(rel, "skills/")
	if !ok {
		return "", "", false
	}
	name, rest, ok = strings.Cut(after, "/")
	return name, rest, ok
}

// cleanRel refuses names a layout could misread: empty, "." or ".." segments, backslashes,
// control characters and over-long paths.
func cleanRel(rel string) bool {
	if len(rel) > 1024 || strings.Contains(rel, `\`) {
		return false
	}
	for _, r := range rel {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// Bundle writes files as a deterministic tar: entries in the given order, regular files only,
// mode 0444 or 0555, no owner and the Unix epoch as every time. The digest is "sha256:<hex>" of
// the tar bytes, so the same files always give the same digest.
func Bundle(files []File) ([]byte, string, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	epoch := time.Unix(0, 0).UTC()
	for _, f := range files {
		mode := int64(0o444)
		if f.Executable {
			mode = 0o555
		}
		h := &tar.Header{
			Typeflag: tar.TypeReg, Name: f.Path, Mode: mode, Size: int64(len(f.Data)),
			ModTime: epoch, Format: tar.FormatPAX,
		}
		if err := tw.WriteHeader(h); err != nil {
			return nil, "", err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, "", err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), "sha256:" + hex.EncodeToString(sum[:]), nil
}

// BundleFile returns the file at path in a bundle, or nil when the bundle has none.
func BundleFile(bundle []byte, path string) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(bundle))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Name == path {
			return io.ReadAll(tr)
		}
	}
}
