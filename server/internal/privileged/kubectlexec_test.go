package privileged_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/oldsj/mainloop/server/internal/privileged"
)

// stand-in kubectl: prints its environment, its kubeconfig's mode, its args and stdin, then exits
// as the first arg asks.
const fakeKubectlScript = `#!/bin/sh
echo "env: $(env | sort | tr '\n' ' ')"
echo "mode: $(stat -c %a "$KUBECONFIG" 2>/dev/null || stat -f %Lp "$KUBECONFIG")"
grep -q "token: ` + executorToken + `" "$KUBECONFIG" && echo "token in config"
echo "args: $*"
case "$1" in
  stdin) cat ;;
  fail) echo "boom" >&2; exit 3 ;;
  sleep) exec sleep 5 ;;
  flood) head -c 2000000 /dev/zero | tr '\0' x ;;
esac
`

func execKubectl(t *testing.T) (privileged.ExecKubectl, *int64, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(bin, []byte(fakeKubectlScript), 0o700); err != nil { //nolint:gosec // a test's stand-in binary
		t.Fatal(err)
	}
	kube := fake.NewClientset()
	var ttl int64
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		if a.GetSubresource() != "token" || a.(k8stesting.CreateAction).GetNamespace() != executorNS {
			t.Errorf("token request = %+v", a)
		}
		ttl = *tr.Spec.ExpirationSeconds
		tr.Status.Token = executorToken
		return true, tr, nil
	})
	runs := filepath.Join(dir, "runs")
	if err := os.Mkdir(runs, 0o700); err != nil {
		t.Fatal(err)
	}
	return privileged.ExecKubectl{
		Clients: privileged.KubeClients{Client: kube, Base: &rest.Config{Host: "https://kube.example.test:6443"}, Namespace: executorNS},
		Binary:  bin, Dir: runs,
	}, &ttl, runs
}

func TestExecKubectlRunsWithItsOwnKubeconfig(t *testing.T) {
	k, ttl, runs := execKubectl(t)
	out, err := k.Run(context.Background(), privileged.KubectlRun{Args: []string{"stdin", "-f", "-"}, Stdin: "kind: ConfigMap\n", Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stdout := string(out.Stdout)
	for _, want := range []string{"mode: 600", "token in config", "args: stdin -f -", "kind: ConfigMap", "KUBERC=off", "KUBECTL_NO_PLUGINS=true"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "MAINLOOP") || strings.Contains(stdout, "GOFLAGS") || out.ExitCode != 0 || out.TimedOut {
		t.Errorf("run = %+v\n%s", out, stdout)
	}
	if len(out.Credentials) != 1 || out.Credentials[0] != executorToken {
		t.Errorf("credentials = %v", out.Credentials)
	}
	if *ttl < int64((20*time.Second + 2*time.Minute).Seconds()) || *ttl < 600 {
		t.Errorf("token ttl = %d", *ttl)
	}
	if left, _ := os.ReadDir(runs); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestExecKubectlOutcomes(t *testing.T) {
	k, _, _ := execKubectl(t)
	out, err := k.Run(context.Background(), privileged.KubectlRun{Args: []string{"fail"}, Timeout: 20 * time.Second})
	if err != nil || out.ExitCode != 3 || !strings.Contains(string(out.Stderr), "boom") {
		t.Errorf("fail: %+v, %v", out, err)
	}
	out, err = k.Run(context.Background(), privileged.KubectlRun{Args: []string{"sleep"}, Timeout: 300 * time.Millisecond})
	if err != nil || !out.TimedOut {
		t.Errorf("sleep: %+v, %v", out, err)
	}
	out, err = k.Run(context.Background(), privileged.KubectlRun{Args: []string{"flood"}, Timeout: 20 * time.Second})
	if err != nil || len(out.Stdout) > 1<<20+100 || !strings.Contains(string(out.Stdout), "more bytes not kept") {
		t.Errorf("flood: %d bytes, %v", len(out.Stdout), err)
	}
}
