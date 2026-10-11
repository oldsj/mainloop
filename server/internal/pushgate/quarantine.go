package pushgate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/oldsj/mainloop/server/internal/github"
)

type Limits struct {
	BodyBytes     int64
	CommandBytes  int64
	Objects       int64
	ExpandedBytes int64
	DiskBytes     int64
	Timeout       time.Duration
}

func (l Limits) defaults() Limits {
	if l.BodyBytes <= 0 {
		l.BodyBytes = 256 << 20
	}
	if l.CommandBytes <= 0 {
		l.CommandBytes = 256 << 10
	}
	if l.Objects <= 0 {
		l.Objects = 100000
	}
	if l.ExpandedBytes <= 0 {
		l.ExpandedBytes = 512 << 20
	}
	if l.DiskBytes <= 0 {
		l.DiskBytes = 2 << 30
	}
	if l.Timeout <= 0 {
		l.Timeout = 60 * time.Second
	}
	return l
}

type spool struct {
	root   string
	body   *os.File
	Size   int64
	Digest string
}

func (s *spool) close() { _ = s.body.Close(); _ = os.RemoveAll(s.root) }

func newSpool(ctx context.Context, root string, source io.Reader, l Limits) (*spool, error) {
	if root == "" {
		root = "/var/lib/mainloop/git"
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, denial("disk_unavailable")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, denial("private_spool_required")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Getuid()) {
		return nil, denial("private_spool_required")
	} //nolint:gosec // UID is an OS unsigned 32-bit value
	var fs unix.Statfs_t
	if err = unix.Statfs(root, &fs); err != nil || fs.Type == unix.TMPFS_MAGIC || fs.Type == unix.RAMFS_MAGIC {
		return nil, denial("disk_spool_required")
	}
	if fs.Bavail*uint64(fs.Bsize) < uint64(l.DiskBytes)+(64<<20) { //nolint:gosec // positive statfs block size and normalized positive limits
		return nil, denial("disk_unavailable")
	} //nolint:gosec // positive statfs block size
	work, err := os.MkdirTemp(root, "receive-")
	if err != nil {
		return nil, denial("disk_unavailable")
	}
	body, err := os.OpenFile(filepath.Join(work, "request"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600) //nolint:gosec // private directory created above; never a request path
	if err != nil {
		_ = os.RemoveAll(work)
		return nil, err
	}
	s := &spool{root: work, body: body}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(body, h), io.LimitReader(&contextReader{ctx, source}, l.BodyBytes+1))
	if err != nil {
		s.close()
		return nil, denial("http_disconnected")
	}
	if n > l.BodyBytes || n > l.DiskBytes {
		s.close()
		return nil, denial("body_limit")
	}
	s.Size = n
	s.Digest = hex.EncodeToString(h.Sum(nil))
	if err = body.Chmod(0400); err != nil {
		s.close()
		return nil, err
	}
	if _, err = body.Seek(0, io.SeekStart); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

func (s *spool) verify() error {
	if _, err := s.body.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(h, s.body)
	if err != nil || n != s.Size || hex.EncodeToString(h.Sum(nil)) != s.Digest {
		return denial("body_identity_changed")
	}
	_, err = s.body.Seek(0, io.SeekStart)
	return err
}

type validation struct {
	Objects       int64 `json:"objects"`
	ExpandedBytes int64 `json:"expanded_bytes"`
	DiskBytes     int64 `json:"disk_bytes"`
}

// Upstream performs repository-scoped operations. Read and write are separate methods so a
// quarantine seed cannot accidentally publish. Every implementation must bound its responses.
type Upstream interface {
	Refs(context.Context, github.Repo) (map[string]string, error)
	Seed(context.Context, github.Repo, []string, io.Writer) error
	Push(context.Context, github.Repo, io.Reader) ([]byte, error)
	DefaultBranch(context.Context, github.Repo) (string, error)
}

func (s *spool) validate(ctx context.Context, c commands, repo github.Repo, policy Policy, u Upstream, l Limits, guard func(context.Context) error) (validation, error) {
	var v validation
	if _, err := s.body.Seek(c.Offset, io.SeekStart); err != nil {
		return v, err
	}
	if err := walkPack(&contextReader{ctx, s.body}, l); err != nil {
		return v, err
	}
	if err := guard(ctx); err != nil {
		return v, err
	}
	refs, err := u.Refs(ctx, repo)
	if err != nil {
		return v, err
	}
	old := refs[c.Ref]
	if old == "" {
		old = zeroOID
	}
	if old != c.Old {
		return v, denial("remote_ref_changed")
	}
	base := refs["refs/heads/"+policy.DefaultBranch]
	if base == "" || base == zeroOID {
		return v, denial("default_objects_unavailable")
	}
	if _, err = s.git(ctx, l, nil, 0, "init", "--bare", "--initial-branch=quarantine", "--template="); err != nil {
		return v, err
	}
	seed, err := os.OpenFile(filepath.Join(s.root, "seed.pack"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return v, err
	}
	defer func() { _ = seed.Close() }()
	wants := []string{base}
	if old != zeroOID && old != base {
		wants = append(wants, old)
	}
	if err = guard(ctx); err != nil {
		return v, err
	}
	if err = u.Seed(ctx, repo, wants, &limitedWriter{w: seed, left: min(l.BodyBytes, l.DiskBytes-s.Size)}); err != nil {
		return v, err
	}
	if _, err = seed.Seek(0, io.SeekStart); err != nil {
		return v, err
	}
	if err = walkPack(&contextReader{ctx, seed}, l); err != nil {
		return v, err
	}
	if _, err = seed.Seek(0, io.SeekStart); err != nil {
		return v, err
	}
	if _, err = s.git(ctx, l, seed, 0, "index-pack", "--stdin", "--strict"); err != nil {
		return v, err
	}
	if _, err = s.body.Seek(c.Offset, io.SeekStart); err != nil {
		return v, err
	}
	if _, err = s.git(ctx, l, s.body, 0, "index-pack", "--stdin", "--fix-thin", "--strict"); err != nil {
		return v, err
	}
	kind, err := s.git(ctx, l, nil, 0, "cat-file", "-t", c.New)
	if err != nil {
		return v, err
	}
	if string(kind) != "commit\n" {
		return v, denial("commit_required")
	}
	if _, err = s.git(ctx, l, nil, 0, "fsck", "--strict", "--no-reflogs", "--no-dangling", c.New); err != nil {
		return v, err
	}
	if c.Old != zeroOID {
		if _, err = s.git(ctx, l, nil, 0, "merge-base", "--is-ancestor", c.Old, c.New); err != nil {
			return v, denial("non_fast_forward")
		}
	}
	inventory, err := s.git(ctx, l, nil, l.Objects*80, "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	if err != nil {
		return v, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(inventory)), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			return v, denial("git_inventory")
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || size < 0 || size > l.ExpandedBytes-v.ExpandedBytes {
			return v, denial("validated_object_limit")
		}
		v.Objects++
		v.ExpandedBytes += size
		if v.Objects > l.Objects {
			return v, denial("validated_object_limit")
		}
	}
	v.DiskBytes, err = s.disk(l)
	return v, err
}

func (s *spool) disk(l Limits) (int64, error) {
	var size int64
	err := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			st, err := d.Info()
			if err != nil {
				return err
			}
			size += st.Size()
			if size > l.DiskBytes {
				return denial("disk_limit")
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	var fs unix.Statfs_t
	if err = unix.Statfs(s.root, &fs); err != nil || fs.Bavail*uint64(fs.Bsize) < 64<<20 { //nolint:gosec // a successful statfs reports a positive block size
		return 0, denial("disk_unavailable")
	} //nolint:gosec // positive block size
	return size, nil
}

func (s *spool) git(ctx context.Context, l Limits, in io.Reader, outputLimit int64, args ...string) ([]byte, error) {
	disk, err := s.disk(l)
	if err != nil {
		return nil, err
	}
	if outputLimit <= 0 {
		outputLimit = l.CommandBytes
	}
	ctx, cancel := context.WithTimeout(ctx, l.Timeout)
	defer cancel()
	argv := []string{"--as=1073741824", "--cpu=30", fmt.Sprintf("--fsize=%d", (l.DiskBytes-disk)/4), "--nofile=64", "--", "/usr/bin/git",
		"-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never", "-c", "gc.auto=0", "--git-dir=" + filepath.Join(s.root, "repo.git")}
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, "/usr/bin/prlimit", argv...) //nolint:gosec // argv is internal, client OIDs have been validated; no shell.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "HOME=" + s.root, "XDG_CONFIG_HOME=" + s.root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0"}
	var out, stderr bytes.Buffer
	cmd.Stdin = in
	cmd.Stdout = &limitedWriter{w: &out, left: outputLimit}
	cmd.Stderr = &limitedWriter{w: &stderr, left: l.CommandBytes}
	cmd.WaitDelay = time.Second
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, denial("validation_timeout")
		}
		return nil, denial("git_validation")
	}
	if _, err = s.disk(l); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type limitedWriter struct {
	w    io.Writer
	left int64
}

func (w *limitedWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > w.left {
		return 0, denial("response_limit")
	}
	n, err := w.w.Write(b)
	w.left -= int64(n)
	return n, err
}
