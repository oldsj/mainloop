package privileged

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/siderolabs/go-api-signature/pkg/serviceaccount"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcstatus "google.golang.org/grpc/status"
	"sigs.k8s.io/yaml"
)

// ExecSidero runs omnictl and talosctl against a proxy of the executor's own, on loopback, which
// holds the Omni service-account key (see sideroProxy). The binary gets a key made for the run, a
// configuration naming only the proxy, and trusts only the proxy's certificate, in a fresh directory
// removed after the run.
type ExecSidero struct {
	// Endpoint is Omni's URL (https://omni.example.com).
	Endpoint string
	// Key is the executor's Omni service account.
	Key *serviceaccount.ServiceAccount
	// Clusters are the clusters the executor acts on: omnictl writes and sees only theirs, and
	// talosctl reaches only its target's machines.
	Clusters []string
	// Binaries are the tools' paths; a missing one is found on PATH.
	Binaries map[string]string
	// Dir is where each run's directory is made (the executor's /tmp).
	Dir string
	// Upstream, if set, is the connection to Omni (tests); otherwise each run dials Endpoint.
	Upstream *grpc.ClientConn
}

// DecodeOmniKey reads an Omni service-account key, as omnictl serviceaccount create prints it.
func DecodeOmniKey(b64 string) (*serviceaccount.ServiceAccount, error) {
	sa, err := serviceaccount.Decode(b64)
	if err != nil {
		return nil, errors.New("not an Omni service-account key")
	}
	return sa, nil
}

func (e ExecSidero) dial() (*grpc.ClientConn, error) {
	u, err := url.Parse(e.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("omni endpoint %q is not an https URL", e.Endpoint)
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}
	return grpc.NewClient(host,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(32<<20)),
	)
}

func (e ExecSidero) Run(ctx context.Context, run SideroRun) (CommandOutput, error) {
	if e.Key == nil {
		return CommandOutput{}, errors.New("the executor has no Omni key")
	}
	upstream := e.Upstream
	if upstream == nil {
		conn, err := e.dial()
		if err != nil {
			return CommandOutput{}, err
		}
		defer func() { _ = conn.Close() }()
		upstream = conn
	}
	proxy, err := newSideroProxy(e.Key, upstream, run.Tool, run.Cluster, e.Clusters, run.Preflight, run.Timeout+time.Hour)
	if err != nil {
		return CommandOutput{}, err
	}
	creds := []string{proxy.runKeyB64}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return CommandOutput{Credentials: creds}, fmt.Errorf("sidero proxy: %w", err)
	}
	stop := proxy.serve(ln)
	defer stop()
	addr := "https://" + ln.Addr().String()

	dir, err := os.MkdirTemp(e.Dir, run.Tool+"-")
	if err != nil {
		return CommandOutput{Credentials: creds}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, proxy.caPEM, 0o600); err != nil {
		return CommandOutput{Credentials: creds}, err
	}
	// Only what the tool needs. It trusts only the proxy's certificate, so it cannot reach Omni,
	// or anything else over TLS, but through the proxy; and it holds only the run's key.
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + dir, "XDG_CONFIG_HOME=" + dir, "XDG_DATA_HOME=" + dir,
		"SSL_CERT_FILE=" + ca, "SSL_CERT_DIR=" + dir, "OMNI_SERVICE_ACCOUNT_KEY=" + proxy.runKeyB64,
	}
	switch run.Tool {
	case ToolOmnictl:
		env = append(env, "OMNI_ENDPOINT="+addr, "OMNICONFIG="+filepath.Join(dir, "omniconfig"))
	case ToolTalosctl:
		path := filepath.Join(dir, "talosconfig")
		if err := writeTalosconfig(path, addr, run.Cluster, proxy.caPEM); err != nil {
			return CommandOutput{Credentials: creds}, err
		}
		env = append(env, "TALOSCONFIG="+path)
	default:
		return CommandOutput{Credentials: creds}, fmt.Errorf("unknown tool %q", run.Tool)
	}
	stdinFile := ""
	if run.Stdin != "" {
		stdinFile = "stdin.yaml"
		if err := os.WriteFile(filepath.Join(dir, stdinFile), []byte(run.Stdin), 0o600); err != nil {
			return CommandOutput{Credentials: creds}, err
		}
	}
	bin := e.Binaries[run.Tool]
	if bin == "" {
		bin = run.Tool
	}
	out, err := runBinary(ctx, bin, sideroArgv(run.Argv, stdinFile, run.DryRun), env, dir, "", run.Timeout)
	out.Credentials = creds
	return out, err
}

func (e ExecSidero) CheckUpgrade(ctx context.Context, cluster string, nodes []string, image string) error {
	if e.Key == nil {
		return errors.New("the executor has no Omni key")
	}
	upstream := e.Upstream
	if upstream == nil {
		conn, err := e.dial()
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		upstream = conn
	}
	proxy, err := newSideroProxy(e.Key, upstream, ToolTalosctl, cluster, e.Clusters, true, time.Hour)
	if err != nil {
		return err
	}
	err = proxy.checkUpgradeImage(ctx, nodes, image)
	if s, ok := grpcstatus.FromError(err); ok && s.Code() == codes.PermissionDenied {
		return refuse("%s", s.Message())
	}
	return err
}

// writeTalosconfig writes a talosconfig with one context: the proxy as the endpoint, its
// certificate as the CA, the run's identity, and the target cluster.
func writeTalosconfig(path, endpoint, cluster string, caPEM []byte) error {
	b, err := yaml.Marshal(map[string]any{
		"context": "run",
		"contexts": map[string]any{"run": map[string]any{
			"endpoints": []string{endpoint},
			"ca":        base64.StdEncoding.EncodeToString(caPEM),
			"cluster":   cluster,
			"auth":      map[string]any{"siderov1": map[string]any{"identity": sideroRunIdentity}},
		}},
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
