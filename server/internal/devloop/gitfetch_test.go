package devloop_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/devloop/devlooptest"
	"github.com/oldsj/mainloop/server/internal/github"
)

// scratchRepo makes a tiny real git repository with a server/ and a web/ tree, commits it, and
// returns its HEAD commit and a full packfile of everything reachable from it: the same shape a
// GitSeeder hands FetchArchive, built with real git rather than a fixture.
func scratchRepo(t *testing.T) (sha string, pack []byte) {
	t.Helper()
	sha, _, pack = scratchRepoTwoCommits(t)
	return sha, pack
}

// scratchRepoTwoCommits is [scratchRepo] with a second commit on top that changes server/main.go.
// The pack holds both, so either commit can be fetched from it.
func scratchRepoTwoCommits(t *testing.T) (first, second string, pack []byte) {
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
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "server"), 0o755))
	must(os.MkdirAll(filepath.Join(dir, "web"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "server", "main.go"), []byte("package main\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "server", "main_test.go"), []byte("package main // excluded\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "web", "index.html"), []byte("<html></html>"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "web", "node_modules", "x"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "web", "node_modules", "x", "pkg.json"), []byte("{}"), 0o644))
	run("init", "-q", "-b", "main")
	run("add", ".")
	run("commit", "-q", "-m", "seed")
	first = strings.TrimSpace(run("rev-parse", "HEAD"))
	must(os.WriteFile(filepath.Join(dir, "server", "main.go"), []byte("package main // second\n"), 0o644))
	run("commit", "-q", "-a", "-m", "second")
	second = strings.TrimSpace(run("rev-parse", "HEAD"))

	objects := exec.CommandContext(ctx, "git", "rev-list", "--objects", second)
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
	if err := objects.Start(); err != nil {
		t.Fatal(err)
	}
	if err := packCmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := objects.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := packCmd.Wait(); err != nil {
		t.Fatal(err)
	}
	return first, second, packBuf.Bytes()
}

func TestFetchArchiveUnpacksAndArchivesTheRequestedTrees(t *testing.T) {
	t.Parallel()
	sha, pack := scratchRepo(t)
	seeder := &devlooptest.Seeder{Payload: pack}
	out, err := devloop.FetchArchive(context.Background(), seeder, github.Repo{Owner: "o", Name: "n"}, sha, []string{"server", "web"})
	if err != nil {
		t.Fatal(err)
	}
	if seeder.Calls != 1 || seeder.LastSHA != sha {
		t.Fatalf("seeder called with %d calls, last %q, want 1 call for %q", seeder.Calls, seeder.LastSHA, sha)
	}
	names := tarNames(t, out)
	for _, want := range []string{"server/main.go", "web/index.html"} {
		if !names[want] {
			t.Errorf("archive missing %q, has %v", want, names)
		}
	}
}

func TestFilterTarRootsAndExcludes(t *testing.T) {
	t.Parallel()
	sha, pack := scratchRepo(t)
	seeder := &devlooptest.Seeder{Payload: pack}
	full, err := devloop.FetchArchive(context.Background(), seeder, github.Repo{Owner: "o", Name: "n"}, sha, []string{"server", "web"})
	if err != nil {
		t.Fatal(err)
	}
	serverTar, err := devloop.FilterTar(bytes.NewReader(full), "server", []string{"*_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	serverNames := tarNames(t, serverTar)
	if !serverNames["main.go"] {
		t.Errorf("filtered server tar missing main.go, has %v", serverNames)
	}
	if serverNames["main_test.go"] {
		t.Errorf("filtered server tar kept an excluded test file: %v", serverNames)
	}
	if serverNames["server/main.go"] {
		t.Errorf("filtered server tar was not rooted (still has the server/ prefix): %v", serverNames)
	}

	webTar, err := devloop.FilterTar(bytes.NewReader(full), "web", []string{"node_modules/"})
	if err != nil {
		t.Fatal(err)
	}
	webNames := tarNames(t, webTar)
	if !webNames["index.html"] {
		t.Errorf("filtered web tar missing index.html, has %v", webNames)
	}
	for name := range webNames {
		if strings.HasPrefix(name, "node_modules/") {
			t.Errorf("filtered web tar kept an excluded directory entry %q", name)
		}
	}
}

func tarNames(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = true
	}
	return out
}
