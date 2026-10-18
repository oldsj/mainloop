// Package devlooptest fakes devloop's Kubernetes and GitHub dependencies for tests: no
// real cluster, no real GitHub call.
package devlooptest

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/github"
)

// Pods is a fake [devloop.PodClient]. AllPods answers ListPods("") (dev_status); BySelector
// answers a selector-scoped ListPods, as OperationsService always calls it when copying. Queue a
// copy outcome per container with QueueCopyResult; with none queued, a tar Exec succeeds and the
// tar it received is recorded in Received. A health Exec (curl) fails its first HealthFailFirst
// calls, then succeeds, and is counted in HealthCalls.
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
	// CopyHook, when set, runs at the start of every tar Exec, before the fake records it and
	// without its lock held, so a test can block a copy or count copies in flight.
	CopyHook func(ctx context.Context, container string)
	queue    map[string][]error
}

func New() *Pods {
	return &Pods{
		BySelector: map[string][]devloop.PodStatus{}, LogLines: map[string]string{},
		Received: map[string][]byte{}, CopyCalls: map[string]int{}, queue: map[string][]error{},
	}
}

// QueueCopyResult makes the next CopyTree exec to container return err (nil for success), one
// queued result per call; once a container's queue is empty, calls succeed.
func (p *Pods) QueueCopyResult(container string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue[container] = append(p.queue[container], err)
}

func (p *Pods) ListPods(_ context.Context, _, labelSelector string) ([]devloop.PodStatus, error) {
	if p.ListErr != nil {
		return nil, p.ListErr
	}
	if labelSelector == "" {
		return p.AllPods, nil
	}
	return p.BySelector[labelSelector], nil
}

func (p *Pods) Logs(_ context.Context, _, pod, container string, _ devloop.LogOptions) (string, error) {
	return p.LogLines[pod+"/"+container], nil
}

func (p *Pods) Exec(ctx context.Context, _, _, container string, cmd []string, stdin io.Reader) (string, string, error) {
	if len(cmd) > 0 && cmd[0] == "tar" && p.CopyHook != nil {
		p.CopyHook(ctx, container)
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(cmd) > 0 && cmd[0] == "curl" {
		p.HealthCalls++
		if p.HealthCalls <= p.HealthFailFirst {
			return "", "curl: (7) Failed to connect", errors.New("command terminated with exit code 7")
		}
		return `{"status":"healthy"}`, "", nil
	}
	p.CopyCalls[container]++
	if len(cmd) > 0 && cmd[0] == "tar" && stdin != nil {
		data, _ := io.ReadAll(stdin)
		p.Received[container] = data
	}
	if q := p.queue[container]; len(q) > 0 {
		err := q[0]
		p.queue[container] = q[1:]
		if err != nil {
			return "", "", err
		}
	}
	return "", "", nil
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
