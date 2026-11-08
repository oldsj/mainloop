package devloop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/oldsj/mainloop/server/internal/github"
)

// GitSeeder streams a packfile of the given commits from GitHub, with a contents:read
// installation token minted and used entirely server-side. [*pushgate.GitHubUpstream] already
// satisfies this (it is how a new workspace's clone is seeded); devloop introduces no second path
// to GitHub, only a local unpack of what it returns.
type GitSeeder interface {
	Seed(ctx context.Context, repo github.Repo, wants []string, out io.Writer) error
}

const (
	gitFetchTimeout = 2 * time.Minute
	maxPackBytes    = 512 << 20
	// maxArchiveBytes bounds the tar `git archive` writes. The packfile cap does not bound it: a
	// small pack of highly compressible blobs expands to an archive many times its size, and the
	// whole archive is held in memory (Mainloop's own server and web trees archive to about 9 MiB).
	maxArchiveBytes = 64 << 20
	// maxGitSmallOutput caps stdout of the git commands that print at most a line (init,
	// index-pack) and how much of any git command's stderr is kept for its error message.
	maxGitSmallOutput = 64 << 10
	gitBinary         = "/usr/bin/git"
)

// FetchArchive fetches commitSHA's packfile from GitHub through seeder and returns a tar archive
// (as `git archive` produces) of the given top-level paths at that commit. It unpacks the
// objects into a scratch bare repository with a local `git` subprocess; no working tree is
// checked out, and the scratch directory is removed before it returns.
//
// GitSeeder.Seed sends no "have" lines, so GitHub answers with every object reachable from
// commitSHA, not a shallow slice: fine for proving the path against a small repository, and a
// known scaling limit worth a follow-up (a shallow protocol v2 negotiation) before this targets a
// large one.
func FetchArchive(ctx context.Context, seeder GitSeeder, repo github.Repo, commitSHA string, paths []string) ([]byte, error) {
	return fetchArchive(ctx, seeder, repo, commitSHA, paths, maxArchiveBytes)
}

func fetchArchive(ctx context.Context, seeder GitSeeder, repo github.Repo, commitSHA string, paths []string, maxArchive int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, gitFetchTimeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "devloop-fetch-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// init takes its target as a plain argument: unlike every other git command here, it does not
	// honor a --git-dir prefix (that would create the repository in the process's own working
	// directory instead of the scratch one).
	if err := initBareRepo(ctx, dir); err != nil {
		return nil, fmt.Errorf("devloop: init scratch repository: %w", err)
	}
	var pack bytes.Buffer
	if err := seeder.Seed(ctx, repo, []string{commitSHA}, &capWriter{buf: &pack, max: maxPackBytes, overErr: fmt.Errorf("devloop: packfile exceeds %d bytes", maxPackBytes)}); err != nil {
		return nil, fmt.Errorf("devloop: seed %s: %w", commitSHA, err)
	}
	if _, err := gitRun(ctx, dir, pack.Bytes(), maxGitSmallOutput, "index-pack", "--stdin"); err != nil {
		return nil, fmt.Errorf("devloop: index packfile for %s: %w", commitSHA, err)
	}
	args := append([]string{"archive", "--format=tar", commitSHA, "--"}, paths...)
	out, err := gitRun(ctx, dir, nil, maxArchive, args...)
	if errors.Is(err, errOutputCapped) {
		return nil, fmt.Errorf("devloop: archive of %s exceeds %d bytes", commitSHA, maxArchive)
	}
	if err != nil {
		return nil, fmt.Errorf("devloop: archive %s: %w", commitSHA, err)
	}
	return out, nil
}

func initBareRepo(ctx context.Context, dir string) error {
	_, err := gitRunArgv(ctx, nil, maxGitSmallOutput, "init", "--bare", "-q", dir)
	return err
}

// gitRun isolates the subprocess the way pushgate's quarantine spool already does for its own
// git calls: a fixed, minimal environment, no shell, and a bounded wait for the process to exit
// after its context ends. Its stdout is capped at maxOut bytes: past that the subprocess's pipe is
// closed, it stops, and the call fails with errOutputCapped.
func gitRun(ctx context.Context, gitDir string, stdin []byte, maxOut int, args ...string) ([]byte, error) {
	return gitRunArgv(ctx, stdin, maxOut, append([]string{"--git-dir=" + gitDir}, args...)...)
}

func gitRunArgv(ctx context.Context, stdin []byte, maxOut int, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, gitBinary, args...) //nolint:gosec // fixed binary, internal argv, no shell
	cmd.Env = []string{
		"PATH=/usr/bin:/bin", "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0",
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	out := &capWriter{buf: &stdout, max: maxOut, overErr: errOutputCapped}
	cmd.Stdout, cmd.Stderr = out, &limitedBuffer{buf: &stderr, left: maxGitSmallOutput}
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if out.err != nil {
		return nil, out.err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// errOutputCapped is the error a git subprocess's run returns when its stdout passed the cap.
var errOutputCapped = errors.New("devloop: git output exceeds its cap")

// capWriter stops at max instead of growing without bound; GitHub cannot make this process hold
// more than one bounded packfile, or one bounded archive, in memory. Once over, every later write
// fails with overErr, and err records it for the caller.
type capWriter struct {
	buf     *bytes.Buffer
	max     int
	overErr error
	err     error
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.buf.Len()+len(p) > c.max {
		c.err = c.overErr
		return 0, c.err
	}
	return c.buf.Write(p)
}
