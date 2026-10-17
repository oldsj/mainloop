// Package devlooptest fakes devloop's Kubernetes, GitHub and health dependencies for tests: no
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
// answers a selector-scoped ListPods, as OperationsService always calls it when copying or
// tailing logs. Queue a Copy outcome per container with QueueCopyResult; with none queued, Exec
// succeeds and the tar it received is recorded in Received.
type Pods struct {
	mu         sync.Mutex
	AllPods    []devloop.PodStatus
	BySelector map[string][]devloop.PodStatus
	LogLines   map[string]string // "pod/container" -> lines
	Received   map[string][]byte // container -> last tar it was sent
	CopyCalls  map[string]int    // container -> number of Exec calls
	ListErr    error
	queue      map[string][]error
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

func (p *Pods) Exec(_ context.Context, _, _, container string, cmd []string, stdin io.Reader) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
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

// ErrStreamReset looks like a cut exec stream: devloop's uncertain-copy classifier matches it, so
// a queued ErrStreamReset exercises the "unknown, retry the copy" path.
var ErrStreamReset = errors.New("devlooptest: stream reset by peer")

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

// Health is a fake [devloop.HealthChecker]: it fails the first N calls, then succeeds.
type Health struct {
	mu        sync.Mutex
	FailFirst int
	calls     int
	Err       error
}

func (h *Health) Healthy(context.Context) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if h.Err != nil {
		return false, h.Err
	}
	return h.calls > h.FailFirst, nil
}

// Smoke is a fake [devloop.SmokeRunner].
type Smoke struct {
	Checks []devloop.SmokeCheck
	Failed bool
	Err    error
}

func (s *Smoke) Run(context.Context) ([]devloop.SmokeCheck, bool, error) { return s.Checks, s.Failed, s.Err }

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
