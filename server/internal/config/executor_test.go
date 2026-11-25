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
