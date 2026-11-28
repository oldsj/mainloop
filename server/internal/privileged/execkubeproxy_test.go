package privileged

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	metaGet    = metav1.GetOptions{}
	metaList   = metav1.ListOptions{}
	metaDelete = metav1.DeleteOptions{}
)

func clientcmdTransport(cfg *rest.Config) (http.RoundTripper, error) { return rest.TransportFor(cfg) }

// Given a run's credential and a stand-in API server, the sidecar writes a kubeconfig that names
// only itself and a token of its own. Through it a script reads objects with the real token added
// by the proxy, and the proxy refuses Secrets, impersonation, the executor's and the toolbox's
// namespaces, and a wrong token; port-forward passes where exec-only runs of kubectl refuse it.
func TestExecKubeProxyRun(t *testing.T) {
	api := newRecorder(t, true)
	creds, out := t.TempDir(), t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.srv.Certificate().Raw})
	for name, v := range map[string]string{"token": proxyRealToken, "host": api.srv.URL, "ca.crt": string(ca)} {
		if err := os.WriteFile(filepath.Join(creds, name), []byte(v+"\n"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ExecKubeProxy{CredDir: creds, OutDir: out, Protected: []string{proxyOwnNS, "mainloop-toolbox"}, Addr: "127.0.0.1:0"}.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the proxy did not stop")
		}
	})
	path := filepath.Join(out, ExecKubeConfigFile)
	var raw []byte
	for range 100 {
		var err error
		if raw, err = os.ReadFile(path); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(raw) == 0 {
		t.Fatal("no kubeconfig")
	}
	if strings.Contains(string(raw), proxyRealToken) || strings.Contains(string(raw), api.srv.URL) {
		t.Fatalf("the kubeconfig carries the credential or the API server's address:\n%s", raw)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o444 {
		t.Errorf("kubeconfig mode = %v", fi.Mode())
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().ConfigMaps("widgets").Get(ctx, "settings", metaGet); err != nil {
		t.Fatalf("a read through the proxy: %v", err)
	}
	if got := api.all(); len(got) != 1 || got[0].auth != "Bearer "+proxyRealToken || got[0].impersonate {
		t.Fatalf("the API server saw %+v", got)
	}
	for name, call := range map[string]func() error{
		"secrets":       func() error { _, err := cs.CoreV1().Secrets("widgets").List(ctx, metaList); return err },
		"executor":      func() error { _, err := cs.CoreV1().ConfigMaps(proxyOwnNS).List(ctx, metaList); return err },
		"toolbox":       func() error { _, err := cs.CoreV1().Pods("mainloop-toolbox").List(ctx, metaList); return err },
		"toolbox ns":    func() error { _, err := cs.CoreV1().Namespaces().Get(ctx, "mainloop-toolbox", metaGet); return err },
		"toolbox write": func() error { return cs.CoreV1().Pods("mainloop-toolbox").Delete(ctx, "exec-x", metaDelete) },
	} {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "refused by the executor") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// A request with the wrong token is refused before it goes anywhere.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Host+"/api/v1/namespaces/widgets/configmaps", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rt, err := clientcmdTransport(cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %v", resp.StatusCode)
	}
	for _, r := range api.all() {
		if strings.Contains(r.path, "secrets") || strings.Contains(r.path, "mainloop-toolbox") || strings.Contains(r.path, proxyOwnNS) {
			t.Errorf("the API server saw %s", r.path)
		}
	}
	// A port-forward stream reaches the API server; exec-only (kubectl's proxy) refuses it.
	pf := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/namespaces/widgets/pods/db-0/portforward", nil)
	pf.Header.Set("Upgrade", "SPDY/3.1")
	pf.Header.Set("Connection", "Upgrade")
	if why := (&kubeProxy{own: proxyOwnNS, protected: []string{"mainloop-toolbox"}, portForward: true}).refuse(pf); why != "" {
		t.Errorf("run mode refused port-forward: %s", why)
	}
	if why := (&kubeProxy{own: proxyOwnNS}).refuse(pf); why == "" {
		t.Error("kubectl's proxy lets port-forward through")
	}
	attach := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/namespaces/widgets/pods/db-0/attach", nil)
	attach.Header.Set("Upgrade", "SPDY/3.1")
	attach.Header.Set("Connection", "Upgrade")
	if why := (&kubeProxy{own: proxyOwnNS, portForward: true}).refuse(attach); why == "" {
		t.Error("run mode lets attach through")
	}
}

// The sidecar refuses to start without its credential, or without the namespaces to protect.
func TestExecKubeProxyNeedsItsCredential(t *testing.T) {
	ctx := context.Background()
	if err := (ExecKubeProxy{CredDir: t.TempDir(), OutDir: t.TempDir(), Protected: []string{"x"}}).Run(ctx); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Errorf("no token: %v", err)
	}
	dir := t.TempDir()
	for _, n := range []string{"token", "host", "ca.crt"} {
		_ = os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o444)
	}
	if err := (ExecKubeProxy{CredDir: dir, OutDir: t.TempDir()}).Run(ctx); err == nil || !strings.Contains(err.Error(), "protected namespace") {
		t.Errorf("no protected namespace: %v", err)
	}
}
