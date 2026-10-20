package devloop_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/devloop/devlooptest"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

// shells are the userlands the copy script must work in: this machine's sh with GNU tools (the Go
// server images are Debian), and BusyBox (the alpine web image) when the machine has it.
func shells(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{"gnu": ""}
	bb, err := exec.LookPath("busybox")
	if err != nil {
		t.Log("busybox not installed: the BusyBox variant of the copy script is not exercised here")
		return out
	}
	bin := t.TempDir()
	for _, applet := range []string{"sh", "find", "awk", "sort", "tail", "tar", "rm", "rmdir"} {
		if err := os.Symlink(bb, filepath.Join(bin, applet)); err != nil {
			t.Fatal(err)
		}
	}
	out["busybox"] = bin
	return out
}

// mirrorTar builds the archive CopyTree sends for a tree of files, the way operations.go does.
func mirrorTar(t *testing.T, files map[string]string, exclude []string) []byte {
	t.Helper()
	prefixed := map[string]string{}
	for name, content := range files {
		prefixed["t/"+name] = content
	}
	data, err := devloop.MirrorTar(bytes.NewReader(devlooptest.Tar(prefixed)), "t", exclude)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func tarFile(t *testing.T, data []byte, name string) string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			t.Fatalf("archive has no %q", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == name {
			b, _ := io.ReadAll(tr)
			return string(b)
		}
	}
}

// seed writes files under dir.
func seed(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// walk lists what is under dir, relative to it, sorted: directories when dirsOnly, else the rest.
func walk(t *testing.T, dir string, dirsOnly bool) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() == dirsOnly && path != dir {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func copyLocal(t *testing.T, bin, dir string, exclude []string, data []byte) error {
	t.Helper()
	pods := devlooptest.New()
	pods.LocalDirs = map[string]string{"c": dir}
	pods.LocalBin = bin
	return devloop.CopyTree(context.Background(), pods, "ns", "pod", "c", "/src", exclude, data)
}

func wantTree(t *testing.T, dir string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := walk(t, dir, false); !slices.Equal(got, want) {
		t.Errorf("files under the destination:\n got  %q\n want %q", got, want)
	}
}

var serverExcludes = []string{".git/", "bin/", "tmp/", "testdata/", "*.test", "*_test.go"}

// TestCopyTreeDeletesFilesTheCommitRemoved is the review's P2: the destination holds what the image
// or an earlier sync left, and a copy must leave it equal to the commit, not the commit laid over
// the old files. A deleted migration, a deleted Go file and a renamed one must all be gone, in
// every userland the script runs in.
func TestCopyTreeDeletesFilesTheCommitRemoved(t *testing.T) {
	t.Parallel()
	for name, bin := range shells(t) {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			seed(t, dir, map[string]string{
				"go.mod":                             "module x\n",
				"keep.go":                            "package x\n",
				"gone.go":                            "package x\n",
				"pkg/old_name.go":                    "package pkg\n",
				"internal/db/migrations/00270_x.sql": "-- abandoned branch\n",
				"internal/db/migrations/00260_y.sql": "-- on main\n",
				"image_only/file.go":                 "package imageonly\n",
				".devloop-commit":                    "previous-sha",
			})
			data := mirrorTar(t, map[string]string{
				"go.mod":                             "module x\n",
				"keep.go":                            "package x // changed\n",
				"pkg/new_name.go":                    "package pkg\n",
				"internal/db/migrations/00260_y.sql": "-- on main\n",
			}, serverExcludes)
			if err := copyLocal(t, bin, dir, serverExcludes, data); err != nil {
				t.Fatal(err)
			}
			wantTree(t, dir, "go.mod", "keep.go", "pkg/new_name.go", "internal/db/migrations/00260_y.sql")
			if got, _ := os.ReadFile(filepath.Join(dir, "keep.go")); string(got) != "package x // changed\n" {
				t.Errorf("keep.go = %q, want the commit's content", got)
			}
			if got := walk(t, dir, true); !slices.Equal(got, []string{"internal", "internal/db", "internal/db/migrations", "pkg"}) {
				t.Errorf("directories after the copy = %q, want the emptied image_only/ removed and nothing else", got)
			}
			// A second copy of the same commit changes nothing: it is safe to retry.
			if err := copyLocal(t, bin, dir, serverExcludes, data); err != nil {
				t.Fatalf("repeat copy: %v", err)
			}
			wantTree(t, dir, "go.mod", "keep.go", "pkg/new_name.go", "internal/db/migrations/00260_y.sql")
		})
	}
}

// TestCopyTreeKeepsExcludedPaths proves the copy deletes only what the sync manages: build caches,
// dependencies and the other excluded paths in the destination are not in the archive and must
// survive, whether an excluded directory or a pattern.
func TestCopyTreeKeepsExcludedPaths(t *testing.T) {
	t.Parallel()
	for name, bin := range shells(t) {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			seed(t, dir, map[string]string{
				"bin/mainloop":              "binary",
				"tmp/build/cache":           "cache",
				"testdata/fixture.json":     "{}",
				".git/HEAD":                 "ref",
				"main_test.go":              "package x\n",
				"pkg/deep_test.go":          "package pkg\n",
				"pkg/pkg.test":              "binary",
				"stale_pkg/only_test.go":    "package stale\n", // a stale directory that still holds an excluded file
				"stale_pkg/stale.go":        "package stale\n",
				"pkg/bin/not_top_level.txt": "stale: bin/ excludes only the top-level directory",
				"gone.go":                   "package x\n",
			})
			data := mirrorTar(t, map[string]string{"main.go": "package x\n", "pkg/pkg.go": "package pkg\n"}, serverExcludes)
			if err := copyLocal(t, bin, dir, serverExcludes, data); err != nil {
				t.Fatal(err)
			}
			wantTree(t, dir,
				"main.go", "pkg/pkg.go",
				"bin/mainloop", "tmp/build/cache", "testdata/fixture.json", ".git/HEAD",
				"main_test.go", "pkg/deep_test.go", "pkg/pkg.test", "stale_pkg/only_test.go")
		})
	}
}

// TestCopyTreeRefusesToPruneAroundANewlineName: find prints one path per line, so a name holding a
// newline would reach rm as two paths, the second relative to the wrong place. The script refuses
// before deleting anything, and nothing outside the destination is touched.
func TestCopyTreeRefusesToPruneAroundANewlineName(t *testing.T) {
	t.Parallel()
	for name, bin := range shells(t) {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "dest")
			seed(t, dir, map[string]string{"stale.go": "package x\n", "a\n/outside3/victim": "x", "x\ngo.mod": "x"})
			seed(t, root, map[string]string{"outside3/victim": "must survive"})
			data := mirrorTar(t, map[string]string{"go.mod": "module x\n"}, serverExcludes)
			err := copyLocal(t, bin, dir, serverExcludes, data)
			if err == nil || errors.Is(err, devloop.ErrCopyInterrupted) || !strings.Contains(err.Error(), "newline") {
				t.Fatalf("copy with a newline name = %v, want a definite failure naming the newline", err)
			}
			if got := walk(t, dir, false); !slices.Contains(got, "stale.go") || !slices.Contains(got, "go.mod") {
				t.Errorf("the refused copy deleted files: %q", got)
			}
			if _, err := os.Stat(filepath.Join(root, "outside3", "victim")); err != nil {
				t.Errorf("a file outside the destination was touched: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, ".devloop-manifest")); err == nil {
				t.Log("manifest left behind by the refused copy (the next copy removes it)")
			}
		})
	}
}

// TestCopyTreeRemovesSymlinksWithoutFollowingThem: a stale link to a directory and a stale link to
// a file go, as links; what they point at, outside the destination, stays.
func TestCopyTreeRemovesSymlinksWithoutFollowingThem(t *testing.T) {
	t.Parallel()
	for name, bin := range shells(t) {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "dest")
			seed(t, root, map[string]string{"outdir/inner.go": "outside", "outfile.go": "outside"})
			seed(t, dir, map[string]string{"keep.go": "package x\n"})
			for link, target := range map[string]string{"dirlink": "../outdir", "filelink": "../outfile.go"} {
				if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
					t.Fatal(err)
				}
			}
			data := mirrorTar(t, map[string]string{"keep.go": "package x\n"}, serverExcludes)
			if err := copyLocal(t, bin, dir, serverExcludes, data); err != nil {
				t.Fatal(err)
			}
			wantTree(t, dir, "keep.go")
			for _, link := range []string{"dirlink", "filelink"} {
				if _, err := os.Lstat(filepath.Join(dir, link)); err == nil {
					t.Errorf("stale link %s survived", link)
				}
			}
			for _, f := range []string{"outdir/inner.go", "outfile.go"} {
				if _, err := os.Stat(filepath.Join(root, f)); err != nil {
					t.Errorf("%s outside the destination was deleted: %v", f, err)
				}
			}
		})
	}
}

// TestCopyTreeHandlesAwkwardNames proves names that tempt a shell script (spaces, a leading dash,
// glob characters, non-ASCII) are kept when the commit has them and removed when it does not.
func TestCopyTreeHandlesAwkwardNames(t *testing.T) {
	t.Parallel()
	awkward := []string{"a b.go", "-rf", "[a]*.go", "é.go", "dir with space/-x.go", "$HOME.go", "'quote'.go"}
	for name, bin := range shells(t) {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			stale, kept := map[string]string{}, map[string]string{}
			var want []string
			for _, n := range awkward {
				stale["old/"+n], kept["new/"+n] = "x", "x"
				stale["new/gone "+n] = "x"
				want = append(want, "new/"+n)
			}
			seed(t, dir, stale)
			seed(t, dir, map[string]string{"-n": "x", "unrelated.go": "x"})
			if err := copyLocal(t, bin, dir, serverExcludes, mirrorTar(t, kept, serverExcludes)); err != nil {
				t.Fatal(err)
			}
			wantTree(t, dir, want...)
		})
	}
}

// TestCopyTreeDeletesNothingWhenTheExtractionFails proves a cut or corrupt archive does not prune:
// the destination keeps its old files next to whatever was extracted, and the error says tar failed.
func TestCopyTreeDeletesNothingWhenTheExtractionFails(t *testing.T) {
	t.Parallel()
	for name, bin := range shells(t) {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			seed(t, dir, map[string]string{"old.go": "package x\n"})
			big := strings.Repeat("x", 4000)
			data := mirrorTar(t, map[string]string{"a.go": big, "b.go": big, "c.go": big}, serverExcludes)
			err := copyLocal(t, bin, dir, serverExcludes, data[:len(data)/2])
			if err == nil {
				t.Fatal("a cut archive was accepted")
			}
			if errors.Is(err, devloop.ErrCopyInterrupted) {
				t.Errorf("a tar that ran and failed was reported as an unobserved outcome: %v", err)
			}
			if got := walk(t, dir, false); !slices.Contains(got, "old.go") {
				t.Errorf("the old file was deleted although the extraction failed: %v", got)
			}
		})
	}
}

// TestCopyTreeRefusesAnArchiveWithoutAManifest proves a plain filtered tar cannot make the script
// prune against nothing (which would delete the whole tree).
func TestCopyTreeRefusesAnArchiveWithoutAManifest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seed(t, dir, map[string]string{"old.go": "package x\n"})
	plain, err := devloop.FilterTar(bytes.NewReader(devlooptest.Tar(map[string]string{"t/new.go": "package x\n"})), "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = copyLocal(t, "", dir, serverExcludes, plain)
	if err == nil || !strings.Contains(err.Error(), "no complete manifest") {
		t.Fatalf("copy of a tar without a manifest = %v, want an error naming the missing manifest", err)
	}
	wantTree(t, dir, "old.go", "new.go")
}

// TestCopyTreeFailsClearlyWhenAFileCannotBeRemoved proves a failure in the prune step, after the new
// tree is already in place, is a definite failure that carries the reason, not a success and not an
// unobserved outcome that would be retried.
func TestCopyTreeFailsClearlyWhenAFileCannotBeRemoved(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root can remove files from a read-only directory")
	}
	for name, bin := range shells(t) {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			seed(t, dir, map[string]string{"locked/stale.go": "package x\n", "other_stale.go": "package x\n"})
			if err := os.Chmod(filepath.Join(dir, "locked"), 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "locked"), 0o755) })
			data := mirrorTar(t, map[string]string{"new.go": "package x\n"}, serverExcludes)
			err := copyLocal(t, bin, dir, serverExcludes, data)
			if err == nil || errors.Is(err, devloop.ErrCopyInterrupted) || !strings.Contains(err.Error(), "locked/stale.go") {
				t.Fatalf("copy with an unremovable file = %v, want a definite failure naming the file", err)
			}
			if got := walk(t, dir, false); !slices.Contains(got, "new.go") {
				t.Errorf("the new file is missing after a failed prune: %v", got)
			}
		})
	}
}

func TestMirrorTarListsEveryKeptPathLast(t *testing.T) {
	t.Parallel()
	data := mirrorTar(t, map[string]string{"a.go": "x", "pkg/b.go": "y", "skipped_test.go": "z"}, serverExcludes)
	names := tarNames(t, data)
	for _, want := range []string{"a.go", "pkg/b.go", ".devloop-manifest"} {
		if !names[want] {
			t.Errorf("archive missing %q, has %v", want, names)
		}
	}
	lines := strings.Split(strings.TrimSuffix(tarFile(t, data, ".devloop-manifest"), "\n"), "\n")
	sort.Strings(lines[:len(lines)-1])
	if want := []string{"a.go", "pkg/b.go", "# devloop manifest end"}; !slices.Equal(lines, want) {
		t.Errorf("manifest = %q, want the kept paths (not the excluded test file) and then the end marker", lines)
	}
}

func TestMirrorTarRefusesPathsItCannotList(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"t/new\nline.go", "t/.devloop-manifest"} {
		if _, err := devloop.MirrorTar(bytes.NewReader(devlooptest.Tar(map[string]string{name: "x"})), "t", nil); err == nil {
			t.Errorf("MirrorTar accepted %q", name)
		}
	}
}

// packTrees commits each tree in order (a file missing from a later tree is deleted) in a scratch
// repository and returns the commits and a pack of everything reachable from the last.
func packTrees(t *testing.T, trees ...map[string]string) (shas []string, pack []byte) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = []string{
			"PATH=/usr/bin:/bin", "HOME=" + dir,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("init", "-q", "-b", "main")
	for i, files := range trees {
		run("rm", "-r", "-q", "-f", "--ignore-unmatch", ".")
		seed(t, dir, files)
		run("add", "-A", ".")
		run("commit", "-q", "--allow-empty", "-m", fmt.Sprint("commit ", i))
		shas = append(shas, strings.TrimSpace(run("rev-parse", "HEAD")))
	}
	objects := exec.CommandContext(ctx, "git", "rev-list", "--objects", shas[len(shas)-1])
	objects.Dir = dir
	objOut, err := objects.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	packCmd := exec.CommandContext(ctx, "git", "pack-objects", "--stdout")
	packCmd.Dir = dir
	packCmd.Stdin = objOut
	var packBuf bytes.Buffer
	packCmd.Stdout = &packBuf
	for _, c := range []*exec.Cmd{objects, packCmd} {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []*exec.Cmd{objects, packCmd} {
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	return shas, packBuf.Bytes()
}

// TestSyncLeavesEveryContainerEqualToTheCommit runs two real syncs through the loop: the second
// commit deletes a Go file and a migration, renames a Go file and drops a stylesheet. After it every
// container holds exactly the second commit plus its excluded paths, backend, mcp, git and frontend
// alike, and the copy's manifest is gone.
func TestSyncLeavesEveryContainerEqualToTheCommit(t *testing.T) {
	t.Parallel()
	first := map[string]string{
		"server/go.mod": "module x\n", "server/main.go": "package main\n", "server/old.go": "package main\n",
		"server/pkg/before.go": "package pkg\n", "server/internal/db/migrations/00270_x.sql": "-- abandoned\n",
		"server/internal/db/migrations/00260_y.sql": "-- main\n",
		"web/index.html": "<html></html>", "web/src/old.css": "a{}", "web/src/app.tsx": "export {}\n",
	}
	second := map[string]string{
		"server/go.mod": "module x\n", "server/main.go": "package main // 2\n",
		"server/pkg/after.go": "package pkg\n", "server/internal/db/migrations/00260_y.sql": "-- main\n",
		"web/index.html": "<html></html>", "web/src/app.tsx": "export {}\n",
	}
	shas, pack := packTrees(t, first, second)

	for name, bin := range shells(t) {
		t.Run(name, func(t *testing.T) {
			d := testdb.New(t)
			pods := syncPods()
			pods.LocalBin = bin
			pods.LocalDirs = map[string]string{}
			roots := map[string]string{}
			for _, c := range []string{"backend", "mcp", "git", "frontend"} {
				roots[c] = t.TempDir()
				pods.LocalDirs[c] = roots[c]
				// What the image holds before any sync: a file git never had, and an excluded path.
				kept := "bin/mainloop"
				if c == "frontend" {
					kept = "node_modules/pkg/index.js"
				}
				seed(t, roots[c], map[string]string{"image_only.go": "package main\n", kept: "x"})
			}
			svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
			holdLease(t, d, "session-a")
			runOperations(t, svc, d)

			for i, sha := range shas {
				row := waitSync(t, svc, startSync(t, svc, "session-a", fmt.Sprint("key-mirror-", i), sha).ID)
				if row.State != dbstate.DevloopSucceeded {
					t.Fatalf("sync %d = %s (%v), want succeeded", i, row.State, row.Detail)
				}
			}
			for _, c := range []string{"backend", "mcp", "git"} {
				wantTree(t, roots[c], "go.mod", "main.go", "pkg/after.go", "internal/db/migrations/00260_y.sql", "bin/mainloop")
			}
			wantTree(t, roots["frontend"], "index.html", "src/app.tsx", "node_modules/pkg/index.js")
			if got, _ := os.ReadFile(filepath.Join(roots["backend"], "main.go")); string(got) != "package main // 2\n" {
				t.Errorf("main.go = %q, want the second commit's", got)
			}
		})
	}
}
