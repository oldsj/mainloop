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
	Name         string `json:"name"`
	Phase        string `json:"phase"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restart_count"`
	// Deleting is true once the pod has a deletion timestamp: it is on its way out and must not be
	// picked as a sync or health target.
	Deleting   bool     `json:"deleting,omitempty"`
	Containers []string `json:"containers"`
	// ContainerRestarts is each container's own restart count; RestartCount is their sum.
	ContainerRestarts map[string]int32 `json:"container_restarts,omitempty"`
}

// LogOptions bounds a log read.
type LogOptions struct {
	TailLines int64
	Since     time.Time
	// Previous reads the log of the container's last terminated instance, where a crash shows.
	Previous bool
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
		st := PodStatus{Name: pod.Name, Phase: string(pod.Status.Phase), Deleting: pod.DeletionTimestamp != nil}
		ready := len(pod.Status.ContainerStatuses) > 0
		for _, cs := range pod.Status.ContainerStatuses {
			st.Containers = append(st.Containers, cs.Name)
			st.RestartCount += cs.RestartCount
			if st.ContainerRestarts == nil {
				st.ContainerRestarts = map[string]int32{}
			}
			st.ContainerRestarts[cs.Name] = cs.RestartCount
			ready = ready && cs.Ready
		}
		st.Ready = ready
		out = append(out, st)
	}
	return out, nil
}

func (p *K8sPods) Logs(ctx context.Context, namespace, pod, container string, opts LogOptions) (string, error) {
	po := &corev1.PodLogOptions{Container: container, Previous: opts.Previous}
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

// manifestFile is the entry [MirrorTar] adds as the archive's last, listing every path the archive
// holds, and manifestEnd its final line. The copy script prunes against the manifest and removes it.
const (
	manifestFile = ".devloop-manifest"
	manifestEnd  = "# devloop manifest end"
)

// mirrorScript makes the destination equal to the archive on stdin, apart from the excluded paths:
// it extracts the archive over the destination, then deletes every path under it that is neither in
// the archive's manifest nor excluded, files and links first and then the directories that are left
// empty (a directory still holding an excluded path stays). $1 is the destination and the rest are
// the excludes: "dir/" for a top-level directory, anything else a glob on the base name, the same
// rules as [FilterTar]. Nothing is deleted unless tar succeeded and the manifest arrived whole, so
// a cut or failed extraction leaves the old files in place next to the new ones.
//
// A path with a newline in its name cannot be told apart from two paths in find's output, so the
// script refuses to prune when the destination holds one.
//
// It sticks to POSIX sh and the find, awk, sort and tail that both GNU (the Go server images) and
// BusyBox (the alpine web image) provide, and prints nothing to stderr unless something went wrong.
const mirrorScript = `cd "$1" || exit 1
shift
rm -f ` + manifestFile + `
tar -x -f - || exit $?
[ "$(tail -n 1 ` + manifestFile + ` 2>/dev/null)" = "` + manifestEnd + `" ] || { echo "devloop: the archive carried no complete manifest" >&2; exit 1; }
n=$#
for ex in "$@"; do
  case $ex in
    */) set -- "$@" -o -path "./${ex%/}" ;;
    *) set -- "$@" -o -name "$ex" ;;
  esac
done
shift "$n"
nl='
'
if [ -n "$(find . \( -path ./` + manifestFile + ` "$@" \) -prune -o -name "*$nl*" -print)" ]; then
  echo "devloop: a path under the destination has a newline in its name" >&2; exit 1
fi
find . \( -path ./` + manifestFile + ` "$@" \) -prune -o -print |
  awk 'NR == FNR { keep[$0] = 1; next } $0 != "." && !(substr($0, 3) in keep)' ` + manifestFile + ` - |
  LC_ALL=C sort -r |
  while IFS= read -r p; do
    if [ -d "$p" ] && [ ! -L "$p" ]; then rmdir -- "$p" 2>/dev/null || true; else rm -f -- "$p" || exit 1; fi
  done || exit $?
rm -f ` + manifestFile + `
`

// CopyTree mirrors tarData (from [MirrorTar], rooted at destDir) onto destDir inside one container
// by piping it to [mirrorScript] over exec: new and changed files are written, and files the
// archive does not hold are removed, except the paths exclude names (the same list the archive was
// filtered with). It is the only write devloop makes inside a mainloop-dev pod. An error that wraps
// [ErrCopyInterrupted] means the outcome is unknown; any other error is a definite failure, the
// script's stderr included. Running it again with the same archive is safe.
func CopyTree(ctx context.Context, pods PodClient, namespace, pod, container, destDir string, exclude []string, tarData []byte) error {
	cmd := append([]string{"sh", "-c", mirrorScript, "sh", destDir}, exclude...)
	stdout, stderr, err := pods.Exec(ctx, namespace, pod, container, cmd, bytes.NewReader(tarData))
	if err != nil {
		var exit exitStatuser
		if ctx.Err() == nil && errors.As(err, &exit) {
			return fmt.Errorf("devloop: copy to %s/%s: copy exited with status %d: %s", pod, container, exit.ExitStatus(), strings.TrimSpace(stderr))
		}
		if ctx.Err() != nil {
			return fmt.Errorf("devloop: copy to %s/%s: %w", pod, container, ctx.Err())
		}
		return fmt.Errorf("%w: copy to %s/%s: %w (stdout=%q stderr=%q)", ErrCopyInterrupted, pod, container, err, stdout, stderr)
	}
	if rest := unexpectedTarStderr(stderr); rest != "" {
		return fmt.Errorf("devloop: copy to %s/%s: copy reported: %s", pod, container, rest)
	}
	return nil
}

// harmlessTarWarnings are the stderr lines BusyBox tar (the alpine frontend image) prints while
// still extracting every file and exiting 0: it skips PAX extended headers it does not understand
// and strips a leading "/". Anything else on stderr is a real problem.
var harmlessTarWarnings = []string{
	"tar: warning: skipping header '",
	"tar: removing leading '/' from member names",
	"tar: removing leading '/' from hard link targets",
}

// unexpectedTarStderr returns the stderr lines that are not a known harmless BusyBox warning, joined
// by newlines; empty when every line is harmless or stderr is blank.
func unexpectedTarStderr(stderr string) string {
	var rest []string
lines:
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, w := range harmlessTarWarnings {
			if strings.HasPrefix(line, w) {
				continue lines
			}
		}
		rest = append(rest, line)
	}
	return strings.Join(rest, "\n")
}

// FilterTar rewrites a tar stream (as `git archive` produces) to hold only the entries under
// prefix, rooted at that prefix instead of under it, dropping any entry under one of exclude's
// directories or matching one of its glob patterns. It mirrors the excludePaths devspace.yaml
// already applies to the same two trees.
func FilterTar(src io.Reader, prefix string, exclude []string) ([]byte, error) {
	var out bytes.Buffer
	if err := filterTarTo(&out, src, prefix, exclude, false); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// MirrorTar is [FilterTar] with a manifest of every path it kept appended as the last entry, which
// [CopyTree] needs to delete what the archive no longer holds. A path with a newline in it cannot be
// listed, so it fails the call instead of being copied unmanaged.
func MirrorTar(src io.Reader, prefix string, exclude []string) ([]byte, error) {
	var out bytes.Buffer
	if err := filterTarTo(&out, src, prefix, exclude, true); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func filterTarTo(out io.Writer, src io.Reader, prefix string, exclude []string, manifest bool) error {
	tr := tar.NewReader(src)
	tw := tar.NewWriter(out)
	prefix = strings.TrimSuffix(prefix, "/")
	var list strings.Builder
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel, ok := stripTarPrefix(hdr.Name, prefix)
		if !ok || rel == "" || excludedTarEntry(rel, exclude) {
			continue
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, tr, hdr.Size); err != nil {
				return err
			}
		}
		if manifest {
			name := strings.TrimSuffix(rel, "/")
			if strings.Contains(name, "\n") {
				return fmt.Errorf("devloop: cannot mirror a path with a newline in it: %q", name)
			}
			if name == manifestFile {
				return fmt.Errorf("devloop: the tree holds %s, which the copy reserves", manifestFile)
			}
			list.WriteString(name + "\n")
		}
	}
	if manifest {
		list.WriteString(manifestEnd + "\n")
		if err := tw.WriteHeader(&tar.Header{Name: manifestFile, Mode: 0o644, Size: int64(list.Len()), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := io.WriteString(tw, list.String()); err != nil {
			return err
		}
	}
	return tw.Close()
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
