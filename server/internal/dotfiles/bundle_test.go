package dotfiles_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/dotfiles"
)

// entry is one tar entry, as `git archive` would write it.
type entry struct {
	name     string
	body     string
	mode     int64
	typeflag byte
}

func archive(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// git archive starts with a pax global header carrying the commit id.
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header",
		PAXRecords: map[string]string{"comment": strings.Repeat("a", 40)}, Format: tar.FormatPAX}); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typeflag}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		switch h.Typeflag {
		case tar.TypeReg:
			h.Size = int64(len(e.body))
		case tar.TypeSymlink:
			h.Linkname = e.body
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func paths(files []dotfiles.File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

func refusal(t *testing.T, err error) string {
	t.Helper()
	var r *dotfiles.Refusal
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	return r.Reason
}

func TestSelectKeepsOnlyTheAllowedFilesUnderTheDirectory(t *testing.T) {
	tar := archive(t,
		entry{name: "home/", typeflag: tar.TypeDir, mode: 0o755},
		entry{name: "home/.zshrc", body: "export PATH=x"},
		entry{name: "install.sh", body: "#!/bin/sh", mode: 0o755},
		entry{name: "home/.agents/AGENTS.md", body: "# Owner instructions"},
		entry{name: "home/.agents/.skill-lock.json", body: "{}"},
		entry{name: "home/.agents/skills/review/SKILL.md", body: "---\nname: review\n---"},
		entry{name: "home/.agents/skills/review/scripts/run.sh", body: "echo", mode: 0o755},
		entry{name: "home/.agents/skills/review/agents/openai.yaml", body: "x: 1"},
		// No SKILL.md: not a skill, so none of it is kept.
		entry{name: "home/.agents/skills/notes/README.md", body: "notes"},
		entry{name: "home/.agents/skills/loose.md", body: "not in a skill directory"},
		entry{name: "home/.agents/agents/reviewer.md", body: "subagent"},
		entry{name: "home/.agents/agents/nested/deep.md", body: "too deep"},
		entry{name: "home/.agents/agents/reviewer.toml", body: "not markdown"},
		entry{name: "home/.agents/commands/ship.md", body: "command"},
		// A symlink outside the exported paths is ignored, not refused.
		entry{name: "home/.agents/link", typeflag: tar.TypeSymlink, body: "/etc/passwd"},
		entry{name: "home/.agentsX/AGENTS.md", body: "a sibling directory, not under home/.agents"},
	)
	files, err := dotfiles.Select(bytes.NewReader(tar), "home/.agents", dotfiles.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"AGENTS.md", "agents/reviewer.md", "commands/ship.md",
		"skills/review/SKILL.md", "skills/review/agents/openai.yaml", "skills/review/scripts/run.sh",
	}
	if got := paths(files); !slices.Equal(got, want) {
		t.Fatalf("selected %v\nwant %v", got, want)
	}
	for _, f := range files {
		if f.Executable != (f.Path == "skills/review/scripts/run.sh") {
			t.Errorf("%s executable = %v", f.Path, f.Executable)
		}
	}
}

func TestSelectAtTheRepositoryRoot(t *testing.T) {
	files, err := dotfiles.Select(bytes.NewReader(archive(t,
		entry{name: "AGENTS.md", body: "root"},
		entry{name: "CLAUDE.md", body: "@AGENTS.md"},
		entry{name: "home/.agents/AGENTS.md", body: "not at the root"},
	)), "", dotfiles.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(files); !slices.Equal(got, []string{"AGENTS.md", "CLAUDE.md"}) {
		t.Fatalf("selected %v", got)
	}
}

func TestSelectRefusals(t *testing.T) {
	small := dotfiles.Limits{MaxFiles: 2, MaxFileBytes: 8, MaxTotalBytes: 12}
	for name, tc := range map[string]struct {
		entries []entry
		lim     dotfiles.Limits
		reason  string
	}{
		"symlink where a file is kept": {
			entries: []entry{{name: "AGENTS.md", typeflag: tar.TypeSymlink, body: "/etc/passwd"}},
			lim:     dotfiles.DefaultLimits, reason: dotfiles.ReasonUnsupportedFile,
		},
		"symlink inside a skill": {
			entries: []entry{
				{name: "skills/a/SKILL.md", body: "a"},
				{name: "skills/a/data", typeflag: tar.TypeSymlink, body: "../../../.ssh/id_ed25519"},
			},
			lim: dotfiles.DefaultLimits, reason: dotfiles.ReasonUnsupportedFile,
		},
		"hard link": {
			entries: []entry{{name: "commands/x.md", typeflag: tar.TypeLink, body: "AGENTS.md"}},
			lim:     dotfiles.DefaultLimits, reason: dotfiles.ReasonUnsupportedFile,
		},
		"file over the per-file cap": {
			entries: []entry{{name: "AGENTS.md", body: "123456789"}},
			lim:     small, reason: dotfiles.ReasonOverLimits,
		},
		"files over the total cap": {
			entries: []entry{{name: "AGENTS.md", body: "1234567"}, {name: "CLAUDE.md", body: "1234567"}},
			lim:     small, reason: dotfiles.ReasonOverLimits,
		},
		"too many files": {
			entries: []entry{{name: "AGENTS.md", body: "1"}, {name: "CLAUDE.md", body: "2"}, {name: "commands/a.md", body: "3"}},
			lim:     small, reason: dotfiles.ReasonOverLimits,
		},
		"nothing to export": {
			entries: []entry{{name: "README.md", body: "x"}, {name: "skills/a/notes.md", body: "no SKILL.md"}},
			lim:     dotfiles.DefaultLimits, reason: dotfiles.ReasonNothingToExport,
		},
		"directory that does not exist": {
			entries: []entry{{name: "AGENTS.md", body: "x"}},
			lim:     dotfiles.DefaultLimits, reason: dotfiles.ReasonNothingToExport,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := ""
			if name == "directory that does not exist" {
				dir = "missing"
			}
			_, err := dotfiles.Select(bytes.NewReader(archive(t, tc.entries...)), dir, tc.lim)
			if got := refusal(t, err); got != tc.reason {
				t.Fatalf("reason = %q, want %q", got, tc.reason)
			}
		})
	}
}

func TestBundleIsDeterministicAndReadOnly(t *testing.T) {
	files := []dotfiles.File{
		{Path: "AGENTS.md", Data: []byte("# x")},
		{Path: "skills/a/SKILL.md", Data: []byte("a")},
		{Path: "skills/a/run.sh", Data: []byte("echo"), Executable: true},
	}
	first, digest, err := dotfiles.Bundle(files)
	if err != nil {
		t.Fatal(err)
	}
	second, again, err := dotfiles.Bundle(files)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || digest != again || !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		t.Fatalf("bundles differ or digest malformed: %s, %s", digest, again)
	}
	_, other, _ := dotfiles.Bundle(files[:2])
	if other == digest {
		t.Fatal("different files gave the same digest")
	}

	tr := tar.NewReader(bytes.NewReader(first))
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		want := int64(0o444)
		if h.Name == "skills/a/run.sh" {
			want = 0o555
		}
		if h.Typeflag != tar.TypeReg || h.Mode != want || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || !h.ModTime.Equal(h.ModTime.Truncate(0)) || h.ModTime.Unix() != 0 {
			t.Errorf("%s: type %c mode %o uid %d uname %q mtime %v", h.Name, h.Typeflag, h.Mode, h.Uid, h.Uname, h.ModTime)
		}
	}
	if !slices.Equal(names, []string{"AGENTS.md", "skills/a/SKILL.md", "skills/a/run.sh"}) {
		t.Fatalf("entries %v", names)
	}
}
