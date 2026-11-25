package privileged

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// maxCaptured bounds what is kept of each of a run's streams before it is redacted and cut to
// MaxCommandOutput: a command that prints without end cannot fill the executor's memory.
const maxCaptured = 1 << 20

// ExecKubectl runs the kubectl binary against a proxy of the executor's own, on loopback, which
// holds the token minted for AccountAdmin (see kubeProxy). kubectl's kubeconfig, in a fresh
// directory removed after the run, names only the proxy and a token for this run.
type ExecKubectl struct {
	Clients KubeClients
	// Binary is kubectl's path; "kubectl" finds it on PATH.
	Binary string
	// Dir is where each run's directory is made (the executor's /tmp).
	Dir string
}

func (k ExecKubectl) Run(ctx context.Context, run KubectlRun) (CommandOutput, error) {
	// The token outlives the run, by its deadline and a margin, so it cannot expire mid-command.
	cfg, err := k.Clients.ConfigFor(ctx, AccountAdmin, run.Timeout+2*time.Minute)
	if err != nil {
		return CommandOutput{}, err
	}
	creds := []string{cfg.BearerToken}
	proxy, err := newKubeProxy(cfg, k.Clients.Namespace, run.Preflight)
	if err != nil {
		return CommandOutput{Credentials: creds}, err
	}
	creds = append(creds, proxy.runToken)
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return CommandOutput{Credentials: creds}, fmt.Errorf("kubectl proxy: %w", err)
	}
	srv := &http.Server{Handler: proxy, ReadHeaderTimeout: 30 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = srv.Serve(tls.NewListener(ln, proxy.tls)) }()
	defer func() { _ = srv.Close() }()

	dir, err := os.MkdirTemp(k.Dir, "kubectl-")
	if err != nil {
		return CommandOutput{Credentials: creds}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	kc := clientcmdapi.NewConfig()
	kc.Clusters["target"] = &clientcmdapi.Cluster{Server: "https://" + ln.Addr().String(), CertificateAuthorityData: proxy.caPEM}
	kc.AuthInfos["run"] = &clientcmdapi.AuthInfo{Token: proxy.runToken}
	kc.Contexts["target"] = &clientcmdapi.Context{Cluster: "target", AuthInfo: "run", Namespace: "default"}
	kc.CurrentContext = "target"
	path := filepath.Join(dir, "config")
	if err := clientcmd.WriteToFile(*kc, path); err != nil {
		return CommandOutput{Credentials: creds}, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return CommandOutput{Credentials: creds}, err
	}

	bin := k.Binary
	if bin == "" {
		bin = "kubectl"
	}
	// Only what kubectl needs: no proxy, no plugin path, no kuberc from a home it does not have.
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + dir, "KUBECONFIG=" + path, "KUBERC=off", "KUBECTL_NO_PLUGINS=true"}
	out, err := runBinary(ctx, bin, run.Args, env, dir, run.Stdin, run.Timeout)
	out.Credentials = creds
	return out, err
}

// runBinary runs an owner-approved argv without a shell, in dir, with only env, under timeout, and
// keeps a bounded copy of what it prints.
func runBinary(ctx context.Context, bin string, args, env []string, dir, stdin string, timeout time.Duration) (CommandOutput, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, bin, args...) //nolint:gosec // the owner-approved argv, run without a shell; its parser refused what must not run
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	if stdin != "" {
		cmd.Stdin = bytes.NewReader([]byte(stdin))
	}
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := CommandOutput{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var exit *exec.ExitError
	switch {
	case rctx.Err() != nil && ctx.Err() == nil:
		out.TimedOut = true
		return out, nil
	case ctx.Err() != nil:
		return out, ctx.Err()
	case errors.As(err, &exit):
		out.ExitCode = exit.ExitCode()
		return out, nil
	case err != nil:
		return out, fmt.Errorf("run %s: %w", filepath.Base(bin), err)
	}
	return out, nil
}

// capped keeps the first maxCaptured bytes written to it and counts the rest.
type capped struct {
	buf     bytes.Buffer
	dropped int
}

func (c *capped) Write(p []byte) (int, error) {
	room := maxCaptured - c.buf.Len()
	if room >= len(p) {
		return c.buf.Write(p)
	}
	if room > 0 {
		c.buf.Write(p[:room])
	}
	c.dropped += len(p) - max(room, 0)
	return len(p), nil
}

func (c *capped) Bytes() []byte {
	if c.dropped == 0 {
		return c.buf.Bytes()
	}
	return append(c.buf.Bytes(), fmt.Sprintf("\n[… %d more bytes not kept]", c.dropped)...)
}
