// Package devlooptest fakes devloop's Kubernetes and GitHub dependencies for tests: no
// real cluster, no real GitHub call.
package devlooptest

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/github"
)

// Pods is a fake [devloop.PodClient]. AllPods answers ListPods("") (dev_status); BySelector
// answers a selector-scoped ListPods, as OperationsService always calls it when copying. Queue a
// copy outcome per container with QueueCopyResult; with none queued, a tar Exec succeeds and the
// tar it received is recorded in Received. A health Exec (curl) fails its first HealthFailFirst
// calls, then succeeds, and is counted in HealthCalls; it also exits 42 when the commit marker the
// sync wrote is not the commit it asks about, as the real script does after a restart from the
// image. `go run ./cmd/mainloop migrate up` and `smoke --read-only` (sh -c) succeed with canned
// output unless MigrateErr or SmokeErr is set; every Exec is appended to Events as
// "tar:<container>", "marker", "health", "migrate" or "smoke", in call order. Hook runs at the start
// of each, before it takes effect, so a test can restart or replace a pod mid-sync.
type Pods struct {
	mu         sync.Mutex
	AllPods    []devloop.PodStatus
	BySelector map[string][]devloop.PodStatus
	LogLines   map[string]string // "pod/container" -> lines
	Received   map[string][]byte // container -> last tar it was sent
	CopyCalls  map[string]int    // container -> number of tar Exec calls
	ListErr    error

	HealthFailFirst int
	HealthCalls     int
	// MigrateOut and SmokeOut are the output of a failing run (with MigrateErr or SmokeErr) or the
	// smoke's success output (default: a passing summary line).
	MigrateOut, SmokeOut string
	MigrateErr, SmokeErr error
	Events               []string
	// LastPod is the pod each container's most recent tar Exec went to.
	LastPod map[string]string
	stderr  map[string][]string
	// CopyHook, when set, runs at the start of every tar Exec, before the fake records it and
	// without its lock held, so a test can block a copy or count copies in flight.
	CopyHook func(ctx context.Context, container string)
	// Hook, when set, runs at the start of every Exec with its event name, without the fake's lock
	// held (so it may call SetPods or ResetMarker). It may block until ctx is done.
	Hook func(ctx context.Context, event string)
	// LocalDirs, when it holds a container, makes that container's copy real: the Exec runs the
	// copy script on this machine with the destination swapped for the directory, so a test can
	// look at the files left behind. LocalBin, when set, is a directory of tools (BusyBox applets,
	// say) that replaces PATH for it. The copy is still recorded and queued as for any container.
	LocalDirs map[string]string
	LocalBin  string
	marker    string
	// HealthScripts are the scripts the health probes ran, in order.
	HealthScripts []string
	queue         map[string][]error
}

func New() *Pods {
	return &Pods{
		BySelector: map[string][]devloop.PodStatus{}, LogLines: map[string]string{},
		Received: map[string][]byte{}, CopyCalls: map[string]int{}, queue: map[string][]error{},
		LastPod: map[string]string{}, stderr: map[string][]string{},
	}
}

// QueueCopyResult makes the next CopyTree exec to container return err (nil for success), one
// queued result per call; once a container's queue is empty, calls succeed.
func (p *Pods) QueueCopyResult(container string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue[container] = append(p.queue[container], err)
}

// QueueCopyStderr makes the next successful tar Exec to container exit 0 with stderr on its error
// stream, as BusyBox tar does when it warns and carries on.
func (p *Pods) QueueCopyStderr(container, stderr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stderr[container] = append(p.stderr[container], stderr)
}

// SetPods replaces the pods a selector-scoped ListPods answers.
func (p *Pods) SetPods(selector string, pods []devloop.PodStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.BySelector[selector] = pods
}

// ResetMarker forgets the commit marker, as a container restart from the image does.
func (p *Pods) ResetMarker() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.marker = ""
}

func (p *Pods) ListPods(_ context.Context, _, labelSelector string) ([]devloop.PodStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ListErr != nil {
		return nil, p.ListErr
	}
	if labelSelector == "" {
		return p.AllPods, nil
	}
	return p.BySelector[labelSelector], nil
}

// Logs answers LogLines["pod/container"], or LogLines["pod/container/previous"] for a previous-
// instance read.
func (p *Pods) Logs(_ context.Context, _, pod, container string, opts devloop.LogOptions) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if opts.Previous {
		return p.LogLines[pod+"/"+container+"/previous"], nil
	}
	return p.LogLines[pod+"/"+container], nil
}

// event names what an Exec is: the mirror copy (a sh -c script that runs tar), a sh -c step (marker, health, migrate, smoke), or other.
func event(container string, cmd []string) string {
	if len(cmd) == 0 {
		return "other"
	}
	if cmd[0] == "sh" && len(cmd) > 2 && cmd[1] == "-c" {
		switch script := cmd[2]; {
		case strings.Contains(script, "tar -x -f -"):
			return "tar:" + container
		case strings.HasSuffix(script, "migrate up"):
			return "migrate"
		case strings.Contains(script, " smoke --read-only "):
			return "smoke"
		case strings.Contains(script, "/health"):
			return "health"
		case strings.Contains(script, ".devloop-commit"):
			return "marker"
		}
	}
	return "other"
}

func (p *Pods) Exec(ctx context.Context, _, pod, container string, cmd []string, stdin io.Reader) (string, string, error) {
	ev := event(container, cmd)
	if strings.HasPrefix(ev, "tar:") && p.CopyHook != nil {
		p.CopyHook(ctx, container)
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
	}
	if p.Hook != nil {
		p.Hook(ctx, ev)
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Events = append(p.Events, ev)
	switch ev {
	case "migrate":
		if p.MigrateErr != nil {
			return p.MigrateOut, "", p.MigrateErr
		}
		return "applied 0 migration(s)\n", "", nil
	case "smoke":
		out := p.SmokeOut
		if out == "" && p.SmokeErr == nil {
			out = "ok health\nsummary: 8 ok, 0 fail, 2 skip\n"
		}
		return out, "", p.SmokeErr
	case "marker":
		if len(cmd) > 4 {
			p.marker = cmd[4]
		}
		return "", "", nil
	case "health":
		p.HealthScripts = append(p.HealthScripts, cmd[2])
		if len(cmd) < 5 || cmd[4] != p.marker {
			return "", "marker missing", ExitError{Code: 42, Msg: "command terminated with exit code 42"}
		}
		p.HealthCalls++
		if p.HealthCalls <= p.HealthFailFirst {
			return "", "curl: (7) Failed to connect", errors.New("command terminated with exit code 7")
		}
		return "", "", nil
	case "other":
		return "", "", nil
	}
	p.CopyCalls[container]++
	p.LastPod[container] = pod
	var data []byte
	if stdin != nil {
		data, _ = io.ReadAll(stdin)
		p.Received[container] = data
	}
	if q := p.queue[container]; len(q) > 0 {
		err := q[0]
		p.queue[container] = q[1:]
		if err != nil {
			return "", "", err
		}
	}
	if q := p.stderr[container]; len(q) > 0 {
		p.stderr[container] = q[1:]
		return "", q[0], nil
	}
	if dir, ok := p.LocalDirs[container]; ok {
		return p.runLocal(ctx, dir, cmd, data)
	}
	return "", "", nil
}

// runLocal runs the copy script cmd with its destination argument replaced by dir.
func (p *Pods) runLocal(ctx context.Context, dir string, cmd []string, stdin []byte) (string, string, error) {
	args := append([]string(nil), cmd[1:]...)
	args[3] = dir                                // sh -c <script> sh <dest> <excludes...>
	c := exec.CommandContext(ctx, "sh", args...) //nolint:gosec // the copy script this package built, run on the test machine
	if p.LocalBin != "" {
		c.Path = filepath.Join(p.LocalBin, "sh")
		c.Env = []string{"PATH=" + p.LocalBin}
	}
	c.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		err = ExitError{Code: ee.ExitCode(), Msg: "command terminated with exit code " + strconv.Itoa(ee.ExitCode())}
	}
	return stdout.String(), stderr.String(), err
}

// ErrStreamReset looks like a cut exec stream: it is not an exit status, so devloop treats the
// copy's outcome as unknown, and a queued ErrStreamReset exercises the "retry the copy" path.
var ErrStreamReset = errors.New("devlooptest: stream reset by peer")

// ExitError is a remote command that ran and exited non-zero, as client-go reports one: devloop
// treats it as a definite failure, whatever its message says.
type ExitError struct {
	Code int
	Msg  string
}

func (e ExitError) Error() string   { return e.Msg }
func (e ExitError) ExitStatus() int { return e.Code }

// Seeder is a fake [devloop.GitSeeder]: it hands back a fixed payload for any want, recorded for
// assertions.
type Seeder struct {
	mu      sync.Mutex
	Calls   int
	LastSHA string
	Err     error
	Payload []byte
}

func (s *Seeder) Seed(_ context.Context, _ github.Repo, wants []string, out io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls++
	if len(wants) > 0 {
		s.LastSHA = wants[0]
	}
	if s.Err != nil {
		return s.Err
	}
	_, err := out.Write(s.Payload)
	return err
}

// Tar builds a tar archive from a map of path -> content, for tests that feed a realistic tree to
// FilterTar or a fake Seeder.
func Tar(files map[string]string) []byte {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for name, content := range files {
		_ = w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))})
		_, _ = w.Write([]byte(content))
	}
	_ = w.Close()
	return buf.Bytes()
}

// Lock and Unlock guard the fake's recorded state for a test that reads it while copies may run.
func (p *Pods) Lock()   { p.mu.Lock() }
func (p *Pods) Unlock() { p.mu.Unlock() }

// Copies is how many tar Exec calls container has received.
func (p *Pods) Copies(container string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.CopyCalls[container]
}

// Seen returns the Exec calls recorded so far, in order.
func (p *Pods) Seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.Events...)
}
