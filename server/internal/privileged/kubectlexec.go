package privileged

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// ExecKubectl runs the kubectl binary with a token minted for AccountAdmin, written to a kubeconfig
// of its own in a fresh directory that is removed after the run.
type ExecKubectl struct {
	Clients KubeClients
	// Binary is kubectl's path; "kubectl" finds it on PATH.
	Binary string
	// Dir is where each run's directory is made (the executor's /tmp).
	Dir string
}

func (k ExecKubectl) Run(ctx context.Context, run KubectlRun) (KubectlOutput, error) {
	// The token outlives the run, by its deadline and a margin, so it cannot expire mid-command.
	cfg, err := k.Clients.ConfigFor(ctx, AccountAdmin, run.Timeout+2*time.Minute)
	if err != nil {
		return KubectlOutput{}, err
	}
	creds := []string{cfg.BearerToken}
	dir, err := os.MkdirTemp(k.Dir, "kubectl-")
	if err != nil {
		return KubectlOutput{Credentials: creds}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	kc := clientcmdapi.NewConfig()
	kc.Clusters["target"] = &clientcmdapi.Cluster{
		Server: cfg.Host, CertificateAuthority: cfg.CAFile, CertificateAuthorityData: cfg.CAData,
		TLSServerName: cfg.ServerName,
	}
	kc.AuthInfos["executor"] = &clientcmdapi.AuthInfo{Token: cfg.BearerToken}
	kc.Contexts["target"] = &clientcmdapi.Context{Cluster: "target", AuthInfo: "executor", Namespace: "default"}
	kc.CurrentContext = "target"
	path := filepath.Join(dir, "config")
	if err := clientcmd.WriteToFile(*kc, path); err != nil {
		return KubectlOutput{Credentials: creds}, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return KubectlOutput{Credentials: creds}, err
	}

	rctx, cancel := context.WithTimeout(ctx, run.Timeout)
	defer cancel()
	bin := k.Binary
	if bin == "" {
		bin = "kubectl"
	}
	cmd := exec.CommandContext(rctx, bin, run.Args...) //nolint:gosec // the owner-approved argv, run without a shell; parseKubectl refused what must not run
	cmd.Dir = dir
	// Only what kubectl needs: no proxy, no plugin path, no kuberc from a home it does not have.
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + dir, "KUBECONFIG=" + path, "KUBERC=off", "KUBECTL_NO_PLUGINS=true"}
	cmd.WaitDelay = 5 * time.Second
	if run.Stdin != "" {
		cmd.Stdin = bytes.NewReader([]byte(run.Stdin))
	}
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	out := KubectlOutput{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Credentials: creds}
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
		return out, fmt.Errorf("run kubectl: %w", err)
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
