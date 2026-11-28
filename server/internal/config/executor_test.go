package config_test

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/config"
)

// serve-executor needs only its own settings: no agent token key, a 1Password token, and a
// namespace list that must be names.
func TestParseExecutor(t *testing.T) {
	e, err := config.ParseExecutor(map[string]string{
		"MAINLOOP_EXECUTOR_OP_TOKEN": "fictional", "MAINLOOP_EXECUTOR_SECRET_NAMESPACES": " widgets, , billing ",
		"DB_USER": "mainloop_executor",
	})
	if err != nil || !slices.Equal(e.SecretNamespaces, []string{"widgets", "billing"}) || e.DB.User != "mainloop_executor" {
		t.Fatalf("executor = %+v, %v", e, err)
	}
	if _, err := config.ParseExecutor(map[string]string{}); err == nil || !strings.Contains(err.Error(), "MAINLOOP_EXECUTOR_OP_TOKEN") {
		t.Fatalf("missing token: %v", err)
	}
	if _, err := config.ParseExecutor(map[string]string{"MAINLOOP_EXECUTOR_OP_TOKEN": "x", "MAINLOOP_EXECUTOR_SECRET_NAMESPACES": "Bad_NS"}); err == nil {
		t.Fatal("a bad namespace was accepted")
	}
	if e.Kubernetes {
		t.Fatal("the k8s.* types are on by default")
	}
}

// The k8s.* types need the executor's own namespace, where its narrow ServiceAccounts live.
func TestParseExecutorKubernetes(t *testing.T) {
	on := map[string]string{"MAINLOOP_EXECUTOR_OP_TOKEN": "x", "MAINLOOP_EXECUTOR_KUBERNETES_ENABLED": "true"}
	if _, err := config.ParseExecutor(on); err == nil || !strings.Contains(err.Error(), "MAINLOOP_EXECUTOR_NAMESPACE") {
		t.Fatalf("no namespace: %v", err)
	}
	on["MAINLOOP_EXECUTOR_NAMESPACE"], on["MAINLOOP_EXECUTOR_CLUSTER"] = "mainloop-executor", " internal-01 "
	e, err := config.ParseExecutor(on)
	if err != nil || !e.Kubernetes || e.Namespace != "mainloop-executor" || e.Cluster != "internal-01" {
		t.Fatalf("executor = %+v, %v", e, err)
	}
}

// command.omnictl and command.talosctl need Omni's https URL and the service account's key; the
// clusters talosctl may target must be names.
func TestParseExecutorOmni(t *testing.T) {
	on := map[string]string{"MAINLOOP_EXECUTOR_OP_TOKEN": "x", "MAINLOOP_EXECUTOR_OMNI_ENABLED": "true"}
	_, err := config.ParseExecutor(on)
	if err == nil || !strings.Contains(err.Error(), "MAINLOOP_EXECUTOR_OMNI_ENDPOINT") || !strings.Contains(err.Error(), "MAINLOOP_EXECUTOR_OMNI_KEY") {
		t.Fatalf("no endpoint or key: %v", err)
	}
	for _, bad := range []string{"http://omni.example.com", "omni.example.com", "https://omni.example.com/api", "https://"} {
		on["MAINLOOP_EXECUTOR_OMNI_ENDPOINT"], on["MAINLOOP_EXECUTOR_OMNI_KEY"] = bad, "fictional-key"
		if _, err := config.ParseExecutor(on); err == nil || !strings.Contains(err.Error(), "MAINLOOP_EXECUTOR_OMNI_ENDPOINT") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	on["MAINLOOP_EXECUTOR_OMNI_ENDPOINT"], on["MAINLOOP_EXECUTOR_OMNI_CLUSTERS"] = " https://omni.example.com ", "internal-01, ,lab-02"
	e, err := config.ParseExecutor(on)
	if err != nil || !bool(e.Omni) || e.OmniEndpoint != "https://omni.example.com" || !slices.Equal(e.OmniClusters, []string{"internal-01", "lab-02"}) {
		t.Fatalf("executor = %+v, %v", e, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", e), "fictional-key") {
		t.Error("the executor's settings print the Omni key")
	}
	on["MAINLOOP_EXECUTOR_OMNI_CLUSTERS"] = "Bad_Cluster"
	if _, err := config.ParseExecutor(on); err == nil || !strings.Contains(err.Error(), "MAINLOOP_EXECUTOR_OMNI_CLUSTERS") {
		t.Fatalf("a bad cluster: %v", err)
	}
}

// command.ssh needs at least one host, each a DNS-label name and an IP address with its port.
func TestParseExecutorSSH(t *testing.T) {
	on := map[string]string{"MAINLOOP_EXECUTOR_OP_TOKEN": "x", "MAINLOOP_EXECUTOR_SSH_ENABLED": "true"}
	if _, err := config.ParseExecutor(on); err == nil || !strings.Contains(err.Error(), "MAINLOOP_EXECUTOR_SSH_HOSTS must name a host") {
		t.Fatalf("err = %v", err)
	}
	on["MAINLOOP_EXECUTOR_SSH_HOSTS"] = " nas-01=192.0.2.20:22, ,router=[2001:db8::1]:2222 "
	e, err := config.ParseExecutor(on)
	if err != nil {
		t.Fatal(err)
	}
	want := []config.SSHHost{
		{Name: "nas-01", Address: netip.MustParseAddrPort("192.0.2.20:22")},
		{Name: "router", Address: netip.MustParseAddrPort("[2001:db8::1]:2222")},
	}
	if !slices.Equal(e.SSHHosts, want) || e.SSHDir != "/etc/mainloop-executor/ssh" {
		t.Fatalf("hosts = %+v, dir %q", e.SSHHosts, e.SSHDir)
	}
	for list, msg := range map[string]string{
		"nas-01":                                    "is not name=address:port",
		"NAS=192.0.2.20:22":                         "is not name=address:port",
		"nas-01=nas.example.com:22":                 "the address is an IP address and a port",
		"nas-01=192.0.2.20":                         "the address is an IP address and a port",
		"nas-01=192.0.2.20:0":                       "the address is an IP address and a port",
		"nas-01=192.0.2.20:22,nas-01=192.0.2.21:22": `"nas-01" is named twice`,
	} {
		on["MAINLOOP_EXECUTOR_SSH_HOSTS"] = list
		if _, err := config.ParseExecutor(on); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: err = %v, want %q", list, err, msg)
		}
	}
	on["MAINLOOP_EXECUTOR_SSH_HOSTS"], on["MAINLOOP_EXECUTOR_SSH_DIR"] = "nas-01=192.0.2.20:22", "ssh"
	if _, err := config.ParseExecutor(on); err == nil || !strings.Contains(err.Error(), "MAINLOOP_EXECUTOR_SSH_DIR must be an absolute path") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseExecutorUniFi(t *testing.T) {
	on := map[string]string{"MAINLOOP_EXECUTOR_OP_TOKEN": "x", "MAINLOOP_EXECUTOR_UNIFI_ENABLED": "true", "MAINLOOP_EXECUTOR_UNIFI_ADDRESS": " 192.0.2.2:443 "}
	e, err := config.ParseExecutor(on)
	if err != nil || e.UniFiAddress != netip.MustParseAddrPort("192.0.2.2:443") || e.UniFiDir != "/etc/mainloop-executor/unifi" {
		t.Fatalf("e = %+v, %v", e, err)
	}
	for _, addr := range []string{"", "unifi.example.com:443", "192.0.2.2", "192.0.2.2:0"} {
		on["MAINLOOP_EXECUTOR_UNIFI_ADDRESS"] = addr
		if _, err := config.ParseExecutor(on); err == nil || !strings.Contains(err.Error(), "MAINLOOP_EXECUTOR_UNIFI_ADDRESS must be the console's IP address and port") {
			t.Errorf("%q: err = %v", addr, err)
		}
	}
	on["MAINLOOP_EXECUTOR_UNIFI_ADDRESS"], on["MAINLOOP_EXECUTOR_UNIFI_DIR"] = "192.0.2.2:443", "unifi"
	if _, err := config.ParseExecutor(on); err == nil || !strings.Contains(err.Error(), "MAINLOOP_EXECUTOR_UNIFI_DIR must be an absolute path") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseExecutorToolbox(t *testing.T) {
	const image = "ghcr.io/oldsj/mainloop-toolbox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	on := map[string]string{
		"MAINLOOP_EXECUTOR_OP_TOKEN": "x", "MAINLOOP_EXECUTOR_TOOLBOX_ENABLED": "true",
		"MAINLOOP_EXECUTOR_NAMESPACE": "mainloop-executor",
	}
	for name, tc := range map[string]struct {
		set  map[string]string
		want string
	}{
		"no image":       {nil, "MAINLOOP_EXECUTOR_TOOLBOX_IMAGE must be an image reference pinned by digest"},
		"a tag":          {map[string]string{"MAINLOOP_EXECUTOR_TOOLBOX_IMAGE": "ghcr.io/oldsj/mainloop-toolbox:latest"}, "pinned by digest"},
		"short digest":   {map[string]string{"MAINLOOP_EXECUTOR_TOOLBOX_IMAGE": "ghcr.io/oldsj/mainloop-toolbox@sha256:0123"}, "pinned by digest"},
		"the executor's": {map[string]string{"MAINLOOP_EXECUTOR_TOOLBOX_IMAGE": image, "MAINLOOP_EXECUTOR_TOOLBOX_NAMESPACE": "mainloop-executor"}, "other than the executor's"},
		"bad namespace":  {map[string]string{"MAINLOOP_EXECUTOR_TOOLBOX_IMAGE": image, "MAINLOOP_EXECUTOR_TOOLBOX_NAMESPACE": "Toolbox"}, "other than the executor's"},
		"bad pull":       {map[string]string{"MAINLOOP_EXECUTOR_TOOLBOX_IMAGE": image, "MAINLOOP_EXECUTOR_TOOLBOX_PULL_SECRETS": "ok,Bad_Name"}, "not a Secret name"},
		"no namespace":   {map[string]string{"MAINLOOP_EXECUTOR_TOOLBOX_IMAGE": image, "MAINLOOP_EXECUTOR_NAMESPACE": ""}, "MAINLOOP_EXECUTOR_NAMESPACE must be the executor's namespace"},
	} {
		t.Run(name, func(t *testing.T) {
			vars := map[string]string{}
			for k, v := range on {
				vars[k] = v
			}
			for k, v := range tc.set {
				vars[k] = v
			}
			if _, err := config.ParseExecutor(vars); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	on["MAINLOOP_EXECUTOR_TOOLBOX_IMAGE"] = image
	on["MAINLOOP_EXECUTOR_TOOLBOX_PULL_SECRETS"] = " ghcr-pull, ,other "
	e, err := config.ParseExecutor(on)
	if err != nil {
		t.Fatal(err)
	}
	if e.ToolboxImage != image || e.ToolboxNamespace != "mainloop-toolbox" || !slices.Equal(e.ToolboxPullSecretSet, []string{"ghcr-pull", "other"}) {
		t.Fatalf("toolbox = %q %q %v", e.ToolboxImage, e.ToolboxNamespace, e.ToolboxPullSecretSet)
	}
	if e, err := config.ParseExecutor(map[string]string{"MAINLOOP_EXECUTOR_OP_TOKEN": "x"}); err != nil || e.Toolbox {
		t.Fatalf("off by default: %+v (%v)", e, err)
	}
}

// The approval mode is exactly web or signed, or unset (which the processes refuse to start with
// beside an executor): a mistyped value stops serve-executor instead of choosing one.
func TestParseExecutorApprovalsMode(t *testing.T) {
	for in, want := range map[string]config.ApprovalsMode{"": config.ApprovalsUnset, "web": config.ApprovalsWeb, "signed": config.ApprovalsSigned} {
		e, err := config.ParseExecutor(map[string]string{"MAINLOOP_EXECUTOR_OP_TOKEN": "x", "MAINLOOP_PRIVILEGED_APPROVALS_MODE": in})
		if err != nil || e.ApprovalsMode != want {
			t.Errorf("%q: mode %q, %v", in, e.ApprovalsMode, err)
		}
	}
	for _, bad := range []string{"Web", "true", "required", " signed", "web "} {
		_, err := config.ParseExecutor(map[string]string{"MAINLOOP_EXECUTOR_OP_TOKEN": "x", "MAINLOOP_PRIVILEGED_APPROVALS_MODE": bad})
		if err == nil || !strings.Contains(err.Error(), "MAINLOOP_PRIVILEGED_APPROVALS_MODE") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// Cluster reach names the proxy sidecar's image, pinned by digest, and needs Kubernetes.
func TestParseExecutorToolboxProxy(t *testing.T) {
	const image = "ghcr.io/oldsj/mainloop-toolbox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const proxy = "ghcr.io/oldsj/mainloop-backend@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	on := map[string]string{
		"MAINLOOP_EXECUTOR_OP_TOKEN": "x", "MAINLOOP_EXECUTOR_TOOLBOX_ENABLED": "true", "MAINLOOP_EXECUTOR_TOOLBOX_IMAGE": image,
		"MAINLOOP_EXECUTOR_NAMESPACE": "mainloop-executor", "MAINLOOP_EXECUTOR_KUBERNETES_ENABLED": "true",
		"MAINLOOP_EXECUTOR_TOOLBOX_PROXY_IMAGE": proxy,
	}
	e, err := config.ParseExecutor(on)
	if err != nil || e.ToolboxProxyImage != proxy {
		t.Fatalf("proxy = %q, %v", e.ToolboxProxyImage, err)
	}
	for name, set := range map[string]map[string]string{
		"a tag":         {"MAINLOOP_EXECUTOR_TOOLBOX_PROXY_IMAGE": "ghcr.io/oldsj/mainloop-backend:latest"},
		"no kubernetes": {"MAINLOOP_EXECUTOR_KUBERNETES_ENABLED": "false"},
	} {
		vars := map[string]string{}
		for k, v := range on {
			vars[k] = v
		}
		for k, v := range set {
			vars[k] = v
		}
		if _, err := config.ParseExecutor(vars); err == nil || !strings.Contains(err.Error(), "MAINLOOP_EXECUTOR_TOOLBOX_PROXY_IMAGE") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	delete(on, "MAINLOOP_EXECUTOR_TOOLBOX_PROXY_IMAGE")
	if e, err := config.ParseExecutor(on); err != nil || e.ToolboxProxyImage != "" {
		t.Fatalf("off by default: %q, %v", e.ToolboxProxyImage, err)
	}
}

// The sidecar's settings: defaults for the mount paths, and the protected namespaces required.
func TestParseExecKubeProxy(t *testing.T) {
	p, err := config.ParseExecKubeProxy(map[string]string{"MAINLOOP_EXEC_KUBE_PROTECT": "mainloop-executor, mainloop-toolbox"})
	if err != nil || p.Creds != "/run/exec-kube" || p.Out != "/run/kube" || !slices.Equal(p.Protect, []string{"mainloop-executor", "mainloop-toolbox"}) {
		t.Fatalf("proxy = %+v, %v", p, err)
	}
	for _, vars := range []map[string]string{
		{}, {"MAINLOOP_EXEC_KUBE_PROTECT": "Bad_NS"}, {"MAINLOOP_EXEC_KUBE_PROTECT": "x", "MAINLOOP_EXEC_KUBE_OUT": "run/kube"},
	} {
		if _, err := config.ParseExecKubeProxy(vars); err == nil {
			t.Errorf("%v accepted", vars)
		}
	}
}
