package devloop

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// PodStatus is one pod's observable state, for dev_status.
type PodStatus struct {
	Name         string   `json:"name"`
	Phase        string   `json:"phase"`
	Ready        bool     `json:"ready"`
	RestartCount int32    `json:"restart_count"`
	Containers   []string `json:"containers"`
}

// LogOptions bounds a log read.
type LogOptions struct {
	TailLines int64
	Since     time.Time
}

// maxLogBytes and maxExecOutputBytes bound what a pod can make this process hold in memory. A
// workspace never reaches this code; a slow or hostile mainloop-dev container cannot use this
// path to exhaust the control plane.
const (
	maxLogBytes        = 256 << 10
	maxExecOutputBytes = 256 << 10
)

// PodClient is the Kubernetes surface devloop needs: list pods, read logs, and exec a command
// with stdin/stdout/stderr. Nothing else in Mainloop reaches Kubernetes with this credential; a
// task workspace never receives it. Only [OperationsService] (run from serve-api) holds one.
type PodClient interface {
	ListPods(ctx context.Context, namespace, labelSelector string) ([]PodStatus, error)
	Logs(ctx context.Context, namespace, pod, container string, opts LogOptions) (string, error)
	Exec(ctx context.Context, namespace, pod, container string, cmd []string, stdin io.Reader) (stdout, stderr string, err error)
}

// K8sPods is the real [PodClient], backed by client-go. Build it with [NewInClusterPods]; it is
// never satisfiable outside a Kubernetes pod.
type K8sPods struct {
	clientset kubernetes.Interface
	config    *rest.Config
}

// NewInClusterPods builds a [K8sPods] from the pod's own ServiceAccount. It needs the RBAC
// described in docs/specs/devloop.md: pods get/list/watch, pods/log get, pods/exec create, scoped
// to the mainloop-dev namespace only.
func NewInClusterPods() (*K8sPods, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("devloop: in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("devloop: build clientset: %w", err)
	}
	return &K8sPods{clientset: cs, config: cfg}, nil
}

// NewPods builds a [K8sPods] over an existing clientset and config, for tests that exercise the
// real client-go path against an envtest-style API server. Production code uses
// [NewInClusterPods].
func NewPods(cs kubernetes.Interface, cfg *rest.Config) *K8sPods {
	return &K8sPods{clientset: cs, config: cfg}
}

func (p *K8sPods) ListPods(ctx context.Context, namespace, labelSelector string) ([]PodStatus, error) {
	list, err := p.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	out := make([]PodStatus, 0, len(list.Items))
	for _, pod := range list.Items {
		st := PodStatus{Name: pod.Name, Phase: string(pod.Status.Phase)}
		ready := len(pod.Status.ContainerStatuses) > 0
		for _, cs := range pod.Status.ContainerStatuses {
			st.Containers = append(st.Containers, cs.Name)
			st.RestartCount += cs.RestartCount
			ready = ready && cs.Ready
		}
		st.Ready = ready
		out = append(out, st)
	}
	return out, nil
}

func (p *K8sPods) Logs(ctx context.Context, namespace, pod, container string, opts LogOptions) (string, error) {
	po := &corev1.PodLogOptions{Container: container}
	if opts.TailLines > 0 {
		po.TailLines = &opts.TailLines
	}
	if !opts.Since.IsZero() {
		t := metav1.NewTime(opts.Since)
		po.SinceTime = &t
	}
	stream, err := p.clientset.CoreV1().Pods(namespace).GetLogs(pod, po).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()
	b, err := io.ReadAll(io.LimitReader(stream, maxLogBytes))
	return string(b), err
}

func (p *K8sPods) Exec(ctx context.Context, namespace, pod, container string, cmd []string, stdin io.Reader) (string, string, error) {
	req := p.clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container, Command: cmd, Stdin: stdin != nil, Stdout: true, Stderr: true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(p.config, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("devloop: build executor: %w", err)
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &limitedBuffer{buf: &stdout, left: maxExecOutputBytes},
		Stderr: &limitedBuffer{buf: &stderr, left: maxExecOutputBytes},
	})
	return stdout.String(), stderr.String(), err
}

// limitedBuffer caps what Exec holds in memory; a container that writes without bound cannot make
// this process grow unbounded.
type limitedBuffer struct {
	buf  *bytes.Buffer
	left int
}

// Write reports the full input length so the caller never sees a short write (which remotecommand
// would treat as a stream failure); bytes past the cap are silently discarded.
func (w *limitedBuffer) Write(b []byte) (int, error) {
	if w.left <= 0 {
		return len(b), nil
	}
	n := len(b)
	if n > w.left {
		n = w.left
	}
	written, err := w.buf.Write(b[:n])
	w.left -= written
	return len(b), err
}

// ErrCopyInterrupted marks a copy whose outcome could not be observed: the exec stream ended
// without telling us whether tar on the other side finished extracting. Unlike a git push, a tar
// overwrite of the same commit's tree is idempotent, so the loop resolves it by running the copy
// again rather than first observing remote state. A tar that ran and exited non-zero is not this:
// it is a definite failure.
var ErrCopyInterrupted = errors.New("devloop: copy interrupted")

// exitStatuser is what client-go's exec errors implement when the remote command ran and exited.
type exitStatuser interface{ ExitStatus() int }

// CopyTree extracts tarData (already rooted at destDir, see [FilterTar]) into destDir inside one
// container by piping it to `tar -x`. It is the only write devloop makes inside a mainloop-dev
// pod. An error that wraps [ErrCopyInterrupted] means the outcome is unknown; any other error is a
// definite failure, tar's stderr included.
func CopyTree(ctx context.Context, pods PodClient, namespace, pod, container, destDir string, tarData []byte) error {
	stdout, stderr, err := pods.Exec(ctx, namespace, pod, container,
		[]string{"tar", "-x", "-f", "-", "-C", destDir}, bytes.NewReader(tarData))
	if err != nil {
		var exit exitStatuser
		if ctx.Err() == nil && errors.As(err, &exit) {
			return fmt.Errorf("devloop: copy to %s/%s: tar exited with status %d: %s", pod, container, exit.ExitStatus(), strings.TrimSpace(stderr))
		}
		if ctx.Err() != nil {
			return fmt.Errorf("devloop: copy to %s/%s: %w", pod, container, ctx.Err())
		}
		return fmt.Errorf("%w: copy to %s/%s: %v (stdout=%q stderr=%q)", ErrCopyInterrupted, pod, container, err, stdout, stderr)
	}
	if strings.TrimSpace(stderr) != "" {
		return fmt.Errorf("devloop: copy to %s/%s: tar reported: %s", pod, container, strings.TrimSpace(stderr))
	}
	return nil
}

// FilterTar rewrites a tar stream (as `git archive` produces) to hold only the entries under
// prefix, rooted at that prefix instead of under it, dropping any entry under one of exclude's
// directories or matching one of its glob patterns. It mirrors the excludePaths devspace.yaml
// already applies to the same two trees.
func FilterTar(src io.Reader, prefix string, exclude []string) ([]byte, error) {
	tr := tar.NewReader(src)
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	prefix = strings.TrimSuffix(prefix, "/")
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		rel, ok := stripTarPrefix(hdr.Name, prefix)
		if !ok || rel == "" || excludedTarEntry(rel, exclude) {
			continue
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, tr, hdr.Size); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func stripTarPrefix(name, prefix string) (string, bool) {
	name = strings.TrimPrefix(name, "./")
	switch {
	case name == prefix:
		return "", true
	case strings.HasPrefix(name, prefix+"/"):
		return name[len(prefix)+1:], true
	default:
		return "", false
	}
}

// excludedTarEntry reports whether rel is under one of exclude's directories ("node_modules/") or
// matches one of its glob patterns ("*_test.go"), tested against rel's base name.
func excludedTarEntry(rel string, exclude []string) bool {
	for _, pattern := range exclude {
		if dir, ok := strings.CutSuffix(pattern, "/"); ok {
			if rel == dir || strings.HasPrefix(rel, dir+"/") {
				return true
			}
			continue
		}
		if ok, _ := path.Match(pattern, path.Base(rel)); ok {
			return true
		}
	}
	return false
}
