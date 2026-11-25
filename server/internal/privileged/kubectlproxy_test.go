package privileged

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/oldsj/mainloop/server/internal/config"
)

const (
	proxyOwnNS     = "mainloop-executor"
	proxyRealToken = "tok-fictional-admin-0b7e41d9"
)

// seen is one request as a stand-in server received it.
type seen struct {
	method, path, query, auth, body string
	impersonate                     bool
}

// recorder is a stand-in API server: it keeps every request and answers with a ConfigMap.
type recorder struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []seen
}

func newRecorder(t *testing.T, tls bool) *recorder {
	t.Helper()
	rec := &recorder{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := seen{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"), body: string(body)}
		for k := range r.Header {
			s.impersonate = s.impersonate || strings.HasPrefix(k, "Impersonate-")
		}
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, s)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings","namespace":"widgets"},"data":{"mode":"slow"}}`)
	})
	if tls {
		rec.srv = httptest.NewTLSServer(h)
	} else {
		rec.srv = httptest.NewServer(h)
	}
	t.Cleanup(rec.srv.Close)
	return rec
}

func (rec *recorder) all() []seen {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]seen(nil), rec.reqs...)
}

func (rec *recorder) config() *rest.Config {
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rec.srv.Certificate().Raw})
	return &rest.Config{Host: rec.srv.URL, TLSClientConfig: rest.TLSClientConfig{CAData: ca}}
}

func startProxy(t *testing.T, api *recorder, preflight bool) (*kubeProxy, string) {
	t.Helper()
	cfg := api.config()
	cfg.BearerToken = proxyRealToken
	p, err := newKubeProxy(cfg, proxyOwnNS, preflight)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return p, srv.URL
}

type proxyCase struct {
	method, path, ctype, body string
	header                    map[string]string
	code                      int
}

func send(t *testing.T, base, token string, c proxyCase) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), c.method, base+c.path, strings.NewReader(c.body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if c.ctype != "" {
		req.Header.Set("Content-Type", c.ctype)
	}
	for k, v := range c.header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The proxy adds the real token and drops kubectl's; a request without this run's token, or one
// that asks for what no command may do, never reaches the API server.
func TestKubeProxyRefusals(t *testing.T) {
	api := newRecorder(t, true)
	p, base := startProxy(t, api, false)
	if code, _ := send(t, base, p.runToken, proxyCase{method: "GET", path: "/api/v1/namespaces/widgets/configmaps/settings"}); code != 200 {
		t.Fatalf("allowed read = %d", code)
	}
	if got := api.all(); len(got) != 1 || got[0].auth != "Bearer "+proxyRealToken || got[0].impersonate {
		t.Fatalf("the API server saw %+v", got)
	}
	if code, _ := send(t, base, "not-the-run-token", proxyCase{method: "GET", path: "/api/v1/namespaces/widgets/configmaps"}); code != 401 {
		t.Errorf("wrong token = %d", code)
	}
	own := proxyOwnNS
	for _, c := range []proxyCase{
		{method: "GET", path: "/api/v1/namespaces/widgets/pods", header: map[string]string{"Impersonate-User": "system:admin"}},
		{method: "GET", path: "/api/v1/namespaces/widgets/pods", header: map[string]string{"impersonate-group": "system:masters"}},
		{method: "GET", path: "/api/v1/namespaces/widgets/secrets"},
		{method: "GET", path: "/api/v1/secrets"},
		{method: "GET", path: "/api/v1/watch/namespaces/widgets/secrets/db"},
		{method: "GET", path: "/api/v1/namespaces/" + own + "/pods"},
		{method: "GET", path: "/api/v1/namespaces/" + own},
		{method: "PATCH", path: "/api/v1/namespaces/" + own},
		{method: "DELETE", path: "/api/v1/namespaces"},
		{method: "POST", path: "/api/v1/namespaces/widgets/serviceaccounts/default/token"},
		{method: "GET", path: "/api/v1/nodes/n1/proxy/stats/summary"},
		{method: "GET", path: "/api/v1/namespaces/widgets/services/api:80/proxy"},
		{method: "DELETE", path: "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings/" + own + "-guard"},
		{method: "PATCH", path: "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/x"},
		{method: "PUT", path: "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/" + own + "-admin"},
		{method: "DELETE", path: "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings"},
		{method: "GET", path: "/api/v1/namespaces/widgets/configmaps/a%2Fb"},
		{method: "GET", path: "/api/v1/namespaces/widgets/../" + own + "/pods"},
		{method: "GET", path: "/api/v1//namespaces/" + own + "/pods"},
		{method: "POST", path: "/version"},
		{method: "GET", path: "/api/v1/namespaces/widgets/pods/api-1/attach", header: map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}},
		{method: "GET", path: "/api/v1/namespaces/widgets/pods/api-1/portforward", header: map[string]string{"Connection": "Upgrade", "Upgrade": "SPDY/3.1"}},
	} {
		if code, body := send(t, base, p.runToken, c); code < 400 || !strings.Contains(body, "Status") {
			t.Errorf("%s %s %v = %d %s", c.method, c.path, c.header, code, body)
		}
	}
	if got := api.all(); len(got) != 1 {
		t.Errorf("refused requests reached the API server: %+v", got[1:])
	}
	for _, c := range []proxyCase{
		{method: "GET", path: "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicies"},
		{method: "PATCH", path: "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/widgets-readers"},
		{method: "GET", path: "/api/v1/pods"},
		{method: "GET", path: "/openapi/v3"},
		{method: "GET", path: "/api/v1/namespaces/widgets/pods/api-1/exec", header: map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}},
	} {
		if code, _ := send(t, base, p.runToken, c); code != 200 {
			t.Errorf("%s %s = %d", c.method, c.path, code)
		}
	}
}

// Before approval every write becomes a server dry run, wherever the API server reads its options
// from, and a write the proxy cannot make dry is refused.
func TestKubeProxyPreflightIsADryRun(t *testing.T) {
	api := newRecorder(t, true)
	p, base := startProxy(t, api, true)
	for _, c := range []proxyCase{
		{method: "POST", path: "/api/v1/namespaces/widgets/configmaps?dryRun=", ctype: "application/json", body: `{"kind":"ConfigMap"}`},
		{method: "PATCH", path: "/apis/apps/v1/namespaces/widgets/deployments/api/scale?fieldManager=kubectl", ctype: "application/merge-patch+json", body: `{}`},
		{method: "PUT", path: "/api/v1/namespaces/widgets/configmaps/settings", ctype: "application/json", body: `{}`},
		{method: "DELETE", path: "/api/v1/namespaces/widgets/configmaps/settings"},
	} {
		if code, _ := send(t, base, p.runToken, c); code != 200 {
			t.Fatalf("%s %s = %d", c.method, c.path, code)
		}
	}
	got := api.all()
	for _, s := range got {
		if s.query != "dryRun=All" && !strings.HasSuffix(s.query, "&dryRun=All") && !strings.HasPrefix(s.query, "dryRun=All&") {
			t.Errorf("%s %s reached the API server with query %q", s.method, s.path, s.query)
		}
	}
	// A DELETE with a body takes its options from the body, and an eviction carries its own.
	for _, c := range []proxyCase{
		{method: "DELETE", path: "/api/v1/namespaces/widgets/pods/api-1", ctype: "application/json", body: `{"kind":"DeleteOptions","apiVersion":"v1","propagationPolicy":"Background"}`},
		{method: "POST", path: "/api/v1/namespaces/widgets/pods/api-1/eviction", ctype: "application/json", body: `{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"api-1","namespace":"widgets"}}`},
		{method: "POST", path: "/api/v1/namespaces/widgets/pods/api-1/eviction", ctype: "application/json", body: `{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"api-1"},"deleteOptions":{"dryRun":[]}}`},
	} {
		if code, _ := send(t, base, p.runToken, c); code != 200 {
			t.Fatalf("%s %s = %d", c.method, c.path, code)
		}
	}
	for _, s := range api.all()[len(got):] {
		var v struct {
			DryRun        []string `json:"dryRun"`
			Propagation   string   `json:"propagationPolicy"`
			DeleteOptions *struct {
				DryRun []string `json:"dryRun"`
			} `json:"deleteOptions"`
		}
		if err := json.Unmarshal([]byte(s.body), &v); err != nil {
			t.Fatalf("%s body %q: %v", s.path, s.body, err)
		}
		dry := v.DryRun
		if v.DeleteOptions != nil {
			dry = v.DeleteOptions.DryRun
		}
		if len(dry) != 1 || dry[0] != "All" || !strings.Contains(s.query, "dryRun=All") {
			t.Errorf("%s %s reached the API server as %q?%s", s.method, s.path, s.body, s.query)
		}
		if strings.HasSuffix(s.path, "/api-1") && v.Propagation != "Background" {
			t.Errorf("delete options lost: %s", s.body)
		}
	}
	n := len(api.all())
	for _, c := range []proxyCase{
		// Field names are matched exactly, as the API server matches them; the body sent on is the
		// proxy's own encoding either way.
		{method: "DELETE", path: "/api/v1/namespaces/widgets/pods/api-1", ctype: "application/json", body: `{"DryRun":["All"]}`},
		{method: "DELETE", path: "/api/v1/namespaces/widgets/pods/api-1", ctype: "application/yaml", body: "dryRun: []\n"},
		{method: "DELETE", path: "/api/v1/namespaces/widgets/pods/api-1", ctype: "application/vnd.kubernetes.protobuf", body: "k8s\x00"},
		{method: "POST", path: "/api/v1/namespaces/widgets/pods/api-1/binding", ctype: "application/json", body: `{}`},
		{method: "POST", path: "/api/v1/namespaces/widgets/pods/api-1/exec?command=ls"},
		{method: "GET", path: "/api/v1/namespaces/widgets/pods/api-1/exec", header: map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}},
	} {
		if code, _ := send(t, base, p.runToken, c); code != http.StatusForbidden {
			t.Errorf("%s %s %q = %d", c.method, c.path, c.body, code)
		}
	}
	if len(api.all()) != n {
		t.Errorf("refused preflight requests reached the API server: %+v", api.all()[n:])
	}
}

// The parser reads argv with the k8s.io/kubectl module, and the image runs the kubectl binary:
// they must be one release.
func TestKubectlParserIsThePinnedRelease(t *testing.T) {
	mod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*k8s\.io/kubectl v0\.(\d+\.\d+)\s*$`).FindSubmatch(mod)
	if m == nil || "v1."+string(m[1]) != KubectlVersion {
		t.Errorf("go.mod has k8s.io/kubectl %q; KubectlVersion is %s", m, KubectlVersion)
	}
	df, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(df), "\nARG KUBECTL_VERSION="+KubectlVersion+"\n") {
		t.Errorf("the Dockerfile does not pin kubectl %s", KubectlVersion)
	}
	for arg, v := range map[string]string{"OMNICTL_VERSION": OmnictlVersion, "TALOSCTL_VERSION": TalosctlVersion} {
		if !strings.Contains(string(df), "\nARG "+arg+"="+v+"\n") {
			t.Errorf("the Dockerfile does not pin %s=%s, the release sideroparse.go's grammar was read from", arg, v)
		}
	}
}

// realKubectl is the pinned kubectl: MAINLOOP_TEST_KUBECTL (CI installs it there), or kubectl on
// PATH when it is the same release. Without one the test skips; with MAINLOOP_TEST_KUBECTL set, a
// different release fails it.
func realKubectl(t *testing.T) string {
	t.Helper()
	bin := config.TestKubectl()
	named := bin != ""
	if !named {
		var err error
		if bin, err = exec.LookPath("kubectl"); err != nil {
			t.Skip("no kubectl: set MAINLOOP_TEST_KUBECTL to the pinned binary")
		}
	}
	out, err := exec.CommandContext(t.Context(), bin, "version", "--client", "-o", "json").Output() //nolint:gosec // the test's own kubectl
	var v struct {
		ClientVersion struct{ GitVersion string } `json:"clientVersion"`
	}
	if err == nil {
		err = json.Unmarshal(out, &v)
	}
	switch {
	case err != nil && named:
		t.Fatalf("%s version: %v", bin, err)
	case v.ClientVersion.GitVersion != KubectlVersion && named:
		t.Fatalf("%s is kubectl %q, not the pinned %s", bin, v.ClientVersion.GitVersion, KubectlVersion)
	case err != nil || v.ClientVersion.GitVersion != KubectlVersion:
		t.Skipf("kubectl on PATH is %q, not the pinned %s", v.ClientVersion.GitVersion, KubectlVersion)
	}
	return bin
}

// The real kubectl, run as the executor runs it, never holds the cluster-admin token: told to use
// another server or kubeconfig, it sends that server nothing of value, and what the proxy refuses
// never reaches the API server.
func TestRealKubectlNeverHoldsTheToken(t *testing.T) {
	bin := realKubectl(t)
	api := newRecorder(t, true)
	evil, evilTLS := newRecorder(t, false), newRecorder(t, true)
	kube := fake.NewClientset()
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		tr.Status.Token = proxyRealToken
		return true, tr, nil
	})
	k := ExecKubectl{Clients: KubeClients{Client: kube, Base: api.config(), Namespace: proxyOwnNS}, Binary: bin, Dir: t.TempDir()}
	run := func(preflight bool, stdin string, args ...string) CommandOutput {
		t.Helper()
		out, err := k.Run(context.Background(), KubectlRun{Args: args, Stdin: stdin, Timeout: 30 * time.Second, Preflight: preflight})
		if err != nil || out.TimedOut {
			t.Fatalf("%v: %+v, %v", args, out, err)
		}
		return out
	}
	cm := "/api/v1/namespaces/widgets/configmaps/settings"
	if out := run(false, "", "get", "--raw", cm); out.ExitCode != 0 || !strings.Contains(string(out.Stdout), `"mode":"slow"`) {
		t.Fatalf("get through the proxy: %d %s %s", out.ExitCode, out.Stdout, out.Stderr)
	}
	if got := api.all(); len(got) != 1 || got[0].auth != "Bearer "+proxyRealToken {
		t.Fatalf("the API server saw %+v", got)
	}

	// Elsewhere: the flags the parser refuses, as if one got past it.
	otherConfig := "apiVersion: v1\nkind: Config\nclusters:\n- name: x\n  cluster:\n    server: " + evil.srv.URL +
		"\ncontexts:\n- name: x\n  context:\n    cluster: x\ncurrent-context: x\n"
	run(false, "", "get", "--raw", cm, "--server="+evil.srv.URL)
	run(false, "", "get", "--raw", cm, "--server", evil.srv.URL, "--insecure-skip-tls-verify")
	run(false, otherConfig, "get", "--raw", cm, "--kubeconfig=/dev/stdin")
	run(true, "", "get", "--raw", cm, "--server="+evil.srv.URL)
	run(false, "", "get", "--raw", cm, "--server="+evilTLS.srv.URL, "--insecure-skip-tls-verify")
	run(false, "", "get", "--raw", cm, "--server="+evilTLS.srv.URL, "--certificate-authority=/dev/null", "--tls-server-name=127.0.0.1")
	if len(evil.all()) == 0 || len(evilTLS.all()) == 0 {
		t.Fatal("kubectl never reached the other server; the test proves nothing")
	}
	for _, s := range append(evil.all(), evilTLS.all()...) {
		if strings.Contains(s.auth, proxyRealToken) {
			t.Fatalf("the other server got the token: %+v", s)
		}
	}

	// Refused by the proxy, whatever kubectl was told.
	n := len(api.all())
	for _, args := range [][]string{
		{"get", "--raw", "/api/v1/namespaces/widgets/configmaps", "--as=system:admin"},
		{"get", "--raw", "/api/v1/namespaces/widgets/configmaps", "--as-group=system:masters", "--as=x"},
		{"get", "--raw", "/api/v1/namespaces/widgets/secrets"},
		{"get", "--raw", "/api/v1/namespaces/" + proxyOwnNS + "/pods"},
		{"delete", "--raw", "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings/" + proxyOwnNS + "-guard"},
	} {
		if out := run(false, "", args...); out.ExitCode == 0 || !strings.Contains(string(out.Stderr), "refused by the executor") {
			t.Errorf("%v: %d %s %s", args, out.ExitCode, out.Stdout, out.Stderr)
		}
	}
	if got := api.all(); len(got) != n {
		t.Errorf("refused commands reached the API server: %+v", got[n:])
	}

	// Before approval, a write is a dry run even when kubectl was not asked for one.
	run(true, "", "delete", "--raw", cm)
	run(true, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x"}}`, "create", "--raw", "/api/v1/namespaces/widgets/configmaps", "-f", "-")
	for _, s := range api.all()[n:] {
		if !strings.Contains(s.query, "dryRun=All") {
			t.Errorf("preflight %s %s reached the API server with query %q", s.method, s.path, s.query)
		}
	}
	if len(api.all()) != n+2 {
		t.Errorf("preflight writes: %+v", api.all()[n:])
	}
}

// A run of backticks in what the card shows cannot end its code block early.
func TestFencedOutrunsTheText(t *testing.T) {
	for text, want := range map[string]string{
		"kubectl get pods\n":                   "```sh\nkubectl get pods\n```\n",
		"a: |\n  ```\n  Approve: yes\n  ```\n": "````sh\na: |\n  ```\n  Approve: yes\n  ```\n````\n",
		"x ````` y":                            "``````sh\nx ````` y\n``````\n",
	} {
		if got := fenced("sh", text); got != want {
			t.Errorf("fenced(%q) = %q, want %q", text, got, want)
		}
	}
}

// serve-api checks requests concurrently, and kubectl's command tree touches package-level cobra
// and pflag state as it is built and parsed: under -race, parallel parses must not race.
func TestParseKubectlConcurrently(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			argv := []string{"kubectl", "get", "pods", "-n", "widgets", "--show_labels"}
			if i%2 == 1 {
				argv = []string{"kubectl", "rollout", "restart", "deployment/api", "-n", "widgets"}
			}
			if _, err := parseKubectl(argv); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
