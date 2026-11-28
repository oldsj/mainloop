package privileged

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// The files of the run's credential, in ExecKubeProxy.CredDir: a Secret the executor made for this
// run and mounted only into the proxy's container.
const (
	execKubeToken = "token"
	execKubeHost  = "host"
	execKubeCA    = "ca.crt"
	// ExecKubeConfigFile is the kubeconfig the proxy writes for the script, in ExecKubeProxy.OutDir.
	ExecKubeConfigFile = "kubeconfig"
	// ExecKubeAddr is where the proxy listens: loopback, shared with the script's container.
	ExecKubeAddr = "127.0.0.1:6443"
)

// ExecKubeProxy is `mainloop exec-kube-proxy`, the sidecar of a command.exec run with cluster
// reach. It holds the 10-minute-or-longer cluster-admin token the executor minted for this run
// (CredDir), serves the Kubernetes API on loopback with a token of its own and a certificate made at
// start, and writes the kubeconfig that names only those (OutDir, a volume shared with the
// script's container, which never sees CredDir). It refuses what the executor's kubectl proxy
// refuses, and also the executor's and the toolbox's namespaces, and it lets port-forward through,
// so a script reaches an in-cluster Service.
type ExecKubeProxy struct {
	CredDir string
	OutDir  string
	// Protected are the namespaces no request may name: the executor's and the toolbox's.
	Protected []string
	// Addr is the listen address; ExecKubeAddr when empty.
	Addr string
}

// Run serves until ctx ends. The kubeconfig is written once the proxy listens, so its presence is
// the readiness the script waits for.
func (e ExecKubeProxy) Run(ctx context.Context) error {
	cfg, err := e.restConfig()
	if err != nil {
		return err
	}
	if len(e.Protected) == 0 {
		return errors.New("exec-kube-proxy: no protected namespace named")
	}
	proxy, err := newKubeProxyFor(cfg, e.Protected[0], false, e.Protected[1:], true)
	if err != nil {
		return err
	}
	addr := e.Addr
	if addr == "" {
		addr = ExecKubeAddr
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("exec-kube-proxy: %w", err)
	}
	srv := &http.Server{Handler: proxy, ReadHeaderTimeout: 30 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(tls.NewListener(ln, proxy.tls)) }()
	kc := clientcmdapi.NewConfig()
	kc.Clusters["target"] = &clientcmdapi.Cluster{Server: "https://" + ln.Addr().String(), CertificateAuthorityData: proxy.caPEM}
	kc.AuthInfos["run"] = &clientcmdapi.AuthInfo{Token: proxy.runToken}
	kc.Contexts["target"] = &clientcmdapi.Context{Cluster: "target", AuthInfo: "run", Namespace: "default"}
	kc.CurrentContext = "target"
	path := filepath.Join(e.OutDir, ExecKubeConfigFile)
	// Written whole and moved into place, so the script never reads half of it.
	tmp := path + ".tmp"
	if err := clientcmd.WriteToFile(*kc, tmp); err != nil {
		_ = srv.Close()
		return fmt.Errorf("exec-kube-proxy: kubeconfig: %w", err)
	}
	// The script's container reads it: it names only the proxy and this run's token.
	if err := os.Chmod(tmp, 0o444); err != nil { //nolint:gosec // see above
		_ = srv.Close()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = srv.Close()
		return err
	}
	select {
	case <-ctx.Done():
		_ = srv.Close()
		return nil
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// restConfig reads the run's credential. The token and address must be present; the certificate
// authority is the cluster's, as the executor's own in-cluster configuration has it.
func (e ExecKubeProxy) restConfig() (*rest.Config, error) {
	root, err := os.OpenRoot(e.CredDir)
	if err != nil {
		return nil, fmt.Errorf("exec-kube-proxy: the run's credential: %w", err)
	}
	defer func() { _ = root.Close() }()
	read := func(name string) (string, error) {
		b, err := root.ReadFile(name)
		if err != nil || strings.TrimSpace(string(b)) == "" {
			return "", fmt.Errorf("exec-kube-proxy: the run's credential has no %s", name)
		}
		return strings.TrimSpace(string(b)), nil
	}
	token, err := read(execKubeToken)
	if err != nil {
		return nil, err
	}
	host, err := read(execKubeHost)
	if err != nil {
		return nil, err
	}
	ca, err := read(execKubeCA)
	if err != nil {
		return nil, err
	}
	return &rest.Config{Host: host, BearerToken: token, TLSClientConfig: rest.TLSClientConfig{CAData: []byte(ca)}}, nil
}
