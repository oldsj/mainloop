package dotfiles_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/devloop/devlooptest"
	"github.com/oldsj/mainloop/server/internal/dotfiles"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

// fixtureRepo commits a fictional dotfiles tree with real git and returns the commit and a pack of
// it: what a GitSeeder hands FetchArchive.
func fixtureRepo(t *testing.T, files map[string]string) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	git := func(stdin []byte, args ...string) []byte {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = dir
		cmd.Env = []string{
			"PATH=/usr/bin:/bin:/opt/homebrew/bin", "HOME=" + dir,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		}
		if stdin != nil {
			cmd.Stdin = strings.NewReader(string(stdin))
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return out
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(nil, "init", "-q", "-b", "main")
	git(nil, "add", ".")
	git(nil, "commit", "-q", "-m", "fixture")
	sha := strings.TrimSpace(string(git(nil, "rev-parse", "HEAD")))
	return sha, git(git(nil, "rev-list", "--objects", sha), "pack-objects", "--stdout")
}

type refs struct {
	sha  string
	err  error
	got  string
	repo github.Repo
}

func (r *refs) CommitSHA(_ context.Context, repo github.Repo, ref string) (string, error) {
	r.got, r.repo = ref, repo
	return r.sha, r.err
}

var fixture = map[string]string{
	"README.md":                          "fictional dotfiles",
	"home/.zshrc":                        "alias ll='ls -l'",
	"home/.agents/AGENTS.md":             "# Example owner\nPrefer small changes.",
	"home/.agents/skills/tidy/SKILL.md":  "---\nname: tidy\n---\nTidy up.",
	"home/.agents/commands/standup.md":   "Summarise yesterday.",
	"home/.agents/notes/not-exported.md": "ignored",
}

func TestExportReadsTheCommitOnceAndKeepsIt(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	sha, pack := fixtureRepo(t, fixture)
	seeder := &devlooptest.Seeder{Payload: pack}
	r := &refs{sha: sha}
	e := &dotfiles.Exports{Pool: d.Pool, Refs: r, Seeder: seeder}
	src := dotfiles.Source{Repository: "Example/Dotfiles", Path: "home/.agents"}

	commit, unavailable := e.Resolve(ctx, src)
	if unavailable != nil || commit != sha {
		t.Fatalf("resolve = %q, %+v", commit, unavailable)
	}
	if r.got != "HEAD" || r.repo != (github.Repo{Owner: "Example", Name: "Dotfiles"}) {
		t.Fatalf("resolved %v at %q, want the default branch", r.repo, r.got)
	}

	first, err := e.Export(ctx, src, commit)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != dotfiles.StatusReady || first.Commit != sha || !strings.HasPrefix(first.Digest, "sha256:") {
		t.Fatalf("export = %+v", first)
	}
	if seeder.LastSHA != sha {
		t.Fatalf("fetched %q, want %q", seeder.LastSHA, sha)
	}
	content, ok, err := e.BundleContent(ctx, first.Digest)
	if err != nil || !ok || string(content) != string(first.Bundle) {
		t.Fatalf("stored bundle: ok=%v err=%v", ok, err)
	}

	// The same commit, named with other case, is not read again.
	again, err := e.Export(ctx, dotfiles.Source{Repository: "example/dotfiles", Path: "home/.agents"}, commit)
	if err != nil {
		t.Fatal(err)
	}
	if seeder.Calls != 1 || again.Digest != first.Digest || again.Status != dotfiles.StatusReady {
		t.Fatalf("second export: calls=%d %+v", seeder.Calls, again)
	}
}

// An export stored under an older format is not reused: the same commit is read again under
// [dotfiles.ExportFormat] and stored beside it, and the older bundle stays downloadable for the
// Sessions that hold it.
func TestExportFormatBumpReadsTheSameCommitAgain(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	sha, pack := fixtureRepo(t, fixture)
	seeder := &devlooptest.Seeder{Payload: pack}
	e := &dotfiles.Exports{Pool: d.Pool, Refs: &refs{sha: sha}, Seeder: seeder}
	src := dotfiles.Source{Repository: "example/dotfiles", Path: "home/.agents"}

	// Given the commit's export stored under the previous format, with that format's own bundle.
	const older = "sha256:" + "0000000000000000000000000000000000000000000000000000000000000001"
	if _, err := d.Pool.Exec(ctx, `INSERT INTO dotfiles_bundles (digest, content) VALUES ($1, 'older')`, older); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO dotfiles_exports (repository, commit, path, format, status, digest)
		VALUES ('example/dotfiles', $1, 'home/.agents', $2, 'ready', $3)`, sha, dotfiles.ExportFormat-1, older); err != nil {
		t.Fatal(err)
	}

	// When the commit is exported under the current format.
	got, err := e.Export(ctx, src, sha)
	if err != nil {
		t.Fatal(err)
	}

	// Then it was read again, not taken from the older row.
	if seeder.Calls != 1 || got.Status != dotfiles.StatusReady || got.Digest == older || !strings.HasPrefix(got.Digest, "sha256:") {
		t.Fatalf("export = %+v after %d reads, want a fresh read under format %d", got, seeder.Calls, dotfiles.ExportFormat)
	}
	// And the new export is kept: exporting again reads nothing.
	again, err := e.Export(ctx, src, sha)
	if err != nil || again.Digest != got.Digest || seeder.Calls != 1 {
		t.Fatalf("second export = %+v, %v after %d reads", again, err, seeder.Calls)
	}
	// And both formats' rows and bundles remain.
	var rows int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM dotfiles_exports WHERE commit = $1`, sha).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("export rows = %d, err %v, want one per format", rows, err)
	}
	if content, ok, err := e.BundleContent(ctx, older); err != nil || !ok || string(content) != "older" {
		t.Fatalf("older bundle: ok=%v err=%v", ok, err)
	}
}

func TestExportRefusesASecretAndRemembersTheRefusal(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	withKey := map[string]string{
		"AGENTS.md":                "# Example",
		"skills/deploy/SKILL.md":   "---\nname: deploy\n---",
		"skills/deploy/id_example": "-----BEGIN " + "OPENSSH PRIVATE KEY-----\nAAAA\n",
	}
	sha, pack := fixtureRepo(t, withKey)
	seeder := &devlooptest.Seeder{Payload: pack}
	e := &dotfiles.Exports{Pool: d.Pool, Refs: &refs{sha: sha}, Seeder: seeder}
	src := dotfiles.Source{Repository: "example/dotfiles"}

	for range 2 {
		got, err := e.Export(ctx, src, sha)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != dotfiles.StatusRefused || got.Reason != dotfiles.ReasonSecretFound || got.Digest != "" {
			t.Fatalf("export = %+v", got)
		}
		want := []dotfiles.Finding{{Path: "skills/deploy/id_example", Rule: "private_key"}}
		if len(got.Findings) != 1 || got.Findings[0] != want[0] {
			t.Fatalf("findings = %v", got.Findings)
		}
	}
	if seeder.Calls != 1 {
		t.Fatalf("seeder calls = %d, want the refusal kept", seeder.Calls)
	}
	var bundles int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM dotfiles_bundles`).Scan(&bundles); err != nil || bundles != 0 {
		t.Fatalf("bundles = %d, err %v: a refused export must store no content", bundles, err)
	}
}

func TestExportUnavailableIsNotKept(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	sha, pack := fixtureRepo(t, fixture)
	seeder := &devlooptest.Seeder{Err: errors.New("upstream down")}
	e := &dotfiles.Exports{Pool: d.Pool, Refs: &refs{sha: sha}, Seeder: seeder}
	src := dotfiles.Source{Repository: "example/dotfiles", Path: "home/.agents"}

	got, err := e.Export(ctx, src, sha)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != dotfiles.StatusUnavailable || got.Reason != dotfiles.ReasonRepositoryUnavailable {
		t.Fatalf("export = %+v", got)
	}
	seeder.Err, seeder.Payload = nil, pack
	got, err = e.Export(ctx, src, sha)
	if err != nil || got.Status != dotfiles.StatusReady {
		t.Fatalf("retry = %+v, %v", got, err)
	}
}

func TestResolveReasons(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"app not installed": {github.ErrNotInstalled, dotfiles.ReasonAppNotInstalled},
		"no such ref":       {github.ErrNotFound, dotfiles.ReasonRefNotFound},
		"github down":       {github.ErrUnavailable, dotfiles.ReasonRepositoryUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			e := &dotfiles.Exports{Refs: &refs{err: tc.err}}
			commit, r := e.Resolve(ctx, dotfiles.Source{Repository: "example/dotfiles", Ref: "main"})
			if commit != "" || r == nil || r.Status != dotfiles.StatusUnavailable || r.Reason != tc.reason {
				t.Fatalf("resolve = %q, %+v", commit, r)
			}
		})
	}
}
