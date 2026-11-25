package config_test

import (
	"fmt"
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
