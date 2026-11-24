package privileged_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/oldsj/mainloop/server/internal/privileged"
)

// executorToken is the fake admin token each run holds: the result must never carry it.
const executorToken = "tok-fictional-executor-7f3a9c2e"

// fakeKubectl answers kubectl runs from handle; runs keeps every run, dry runs included.
type fakeKubectl struct {
	runs   []privileged.KubectlRun
	handle func(run privileged.KubectlRun, dry bool) privileged.KubectlOutput
}

func (f *fakeKubectl) Run(_ context.Context, run privileged.KubectlRun) (privileged.KubectlOutput, error) {
	f.runs = append(f.runs, run)
	out := f.handle(run, slices.Contains(run.Args, "--dry-run=server"))
	out.Credentials = []string{executorToken}
	return out, nil
}

// real counts the runs that were not dry runs.
func (f *fakeKubectl) real() int {
	n := 0
	for _, r := range f.runs {
		if !slices.Contains(r.Args, "--dry-run=server") {
			n++
		}
	}
	return n
}

func newKubectlEnv(t *testing.T, handle func(m *memKube, run privileged.KubectlRun, dry bool) privileged.KubectlOutput) (*kubeEnv, *memKube, *fakeKubectl) {
	t.Helper()
	e := newKubeEnv(t)
	m := newMemKube(e)
	m.put(memResources["configmap"], configMap(appNS, "settings", map[string]any{"mode": "slow"}))
	k := &fakeKubectl{handle: func(run privileged.KubectlRun, dry bool) privileged.KubectlOutput { return handle(m, run, dry) }}
	e.ex.Kubectl = k
	return e, m, k
}

// labelSettings is `kubectl label configmap settings tier=gold`: a dry run prints the object with
// -o json; the real run stores it.
func labelSettings(m *memKube, _ privileged.KubectlRun, dry bool) privileged.KubectlOutput {
	o := clone(m.objects[m.key(memResources["configmap"], appNS, "settings")])
	u := &unstructured.Unstructured{Object: o}
	labels := u.GetLabels()
	labels["tier"] = "gold"
	u.SetLabels(labels)
	if dry {
		b, _ := json.Marshal(o)
		return privileged.KubectlOutput{Stdout: b}
	}
	m.put(memResources["configmap"], o)
	return privileged.KubectlOutput{Stdout: []byte("configmap/settings labeled\nAuthorization: Bearer " + executorToken + "\n")}
}

func kubectlArgs(argv ...string) map[string]any {
	return map[string]any{"target": cluster, "argv": append([]string{"kubectl"}, argv...)}
}

func kubectlResult(t *testing.T, a privileged.Action) privileged.KubectlResult {
	t.Helper()
	var res privileged.KubectlResult
	if err := json.Unmarshal(a.Result, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// Given a label change, when main requests it as a command, then the card shows the exact command
// and the server dry run's diff against the live object; the agent's facts name the object only.
// After an Approve the executor runs the dry run again, logs that the command started, runs it
// once and returns its output without the token it held.
func TestKubectlChangeShowsTheDryRunAndRunsOnce(t *testing.T) {
	e, m, k := newKubectlEnv(t, labelSettings)
	a := e.request(t, privileged.TypeKubectl, "kubectl-1", kubectlArgs("label", "configmap", "settings", "-n", appNS, "tier=gold"))
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "Run kubectl label on test-cluster as cluster-admin", "kubectl label configmap settings -n widgets tier=gold",
		"ConfigMap widgets/settings (uid uid-settings)", "+    tier: gold", "runs the dry run again", "not run again",
		"Risk: cluster-admin.")
	if seen := string(privileged.AgentFacts(a.Type, a.Facts)); strings.Contains(seen, "slow") || strings.Contains(seen, "gold") ||
		!strings.Contains(seen, "ConfigMap widgets/settings") {
		t.Errorf("the agent sees %s", seen)
	}
	if k.real() != 0 || len(k.runs) != 2 || !slices.Contains(k.runs[0].Args, "json") {
		t.Fatalf("preflight runs = %+v", k.runs)
	}

	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("after execution: %+v (%v)", a, deref(a.LastError))
	}
	if k.real() != 1 || m.data(t, appNS, "settings")["mode"] != "slow" {
		t.Fatalf("runs = %+v", k.runs)
	}
	last := k.runs[len(k.runs)-1]
	if !slices.Equal(last.Args, []string{"label", "configmap", "settings", "-n", appNS, "tier=gold"}) || last.Timeout != privileged.DefaultCommandTimeout {
		t.Fatalf("command = %+v", last)
	}
	res := kubectlResult(t, a)
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "configmap/settings labeled") || strings.Contains(res.Stdout, executorToken) {
		t.Fatalf("result = %+v", res)
	}
	var started int
	if err := e.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM privileged_action_events WHERE action_id = $1 AND step = 'command_started'`, a.ID).Scan(&started); err != nil || started != 1 {
		t.Fatalf("command_started events = %d (%v)", started, err)
	}
}

// Given an approved command whose object changed after the owner saw it, when the executor runs,
// then the dry run differs and nothing runs.
func TestKubectlIsStaleWhenTheDryRunDiffers(t *testing.T) {
	e, m, k := newKubectlEnv(t, labelSettings)
	a := e.request(t, privileged.TypeKubectl, "kubectl-1", kubectlArgs("label", "configmap", "settings", "-n", appNS, "tier=gold"))
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	moved := configMap(appNS, "settings", map[string]any{"mode": "slow"})
	moved["metadata"].(map[string]any)["labels"].(map[string]any)["tier"] = "silver"
	m.put(memResources["configmap"], moved)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateStale || k.real() != 0 {
		t.Fatalf("action = %+v (%v), real runs %d", a, deref(a.LastError), k.real())
	}
}

// Given an attempt that logged command_started and lost its outcome, when the executor claims the
// action again, then it ends uncertain without running the command a second time.
func TestKubectlNeverRunsTwice(t *testing.T) {
	e, _, k := newKubectlEnv(t, labelSettings)
	a := e.request(t, privileged.TypeKubectl, "kubectl-1", kubectlArgs("label", "configmap", "settings", "-n", appNS, "tier=gold"))
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	e.exec(t, `INSERT INTO privileged_action_events (action_id, step, detail) VALUES ($1, 'command_started', 'kubectl label')`, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateUncertain || k.real() != 0 {
		t.Fatalf("action = %+v (%v), real runs %d", a, deref(a.LastError), k.real())
	}
}

// A read has no preview; its output comes back redacted. A nonzero exit fails with what kubectl
// said; a command past its deadline is uncertain.
func TestKubectlOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		out   privileged.KubectlOutput
		state string
		check func(t *testing.T, res privileged.KubectlResult)
	}{
		"read": {privileged.KubectlOutput{Stdout: []byte("NAME    READY\napi-1   1/1\nseen " + executorToken + " and " + base64.StdEncoding.EncodeToString([]byte(executorToken)) + "\n")}, privileged.StateSucceeded,
			func(t *testing.T, res privileged.KubectlResult) {
				if !strings.Contains(res.Stdout, "api-1   1/1") || !strings.Contains(res.Stdout, "seen [redacted] and [redacted]") {
					t.Errorf("result = %+v", res)
				}
			}},
		"exit 1": {privileged.KubectlOutput{ExitCode: 1, Stderr: []byte(`Error from server (NotFound): pods "api-9" not found`)}, privileged.StateFailed,
			func(t *testing.T, res privileged.KubectlResult) {
				if res.ExitCode != 1 || !strings.Contains(res.Stderr, "NotFound") {
					t.Errorf("result = %+v", res)
				}
			}},
		"timeout": {privileged.KubectlOutput{TimedOut: true, Stdout: []byte("partial")}, privileged.StateUncertain, nil},
	} {
		t.Run(name, func(t *testing.T) {
			e, _, k := newKubectlEnv(t, func(*memKube, privileged.KubectlRun, bool) privileged.KubectlOutput { return tc.out })
			args := kubectlArgs("get", "pods", "-n", appNS)
			args["timeout_seconds"] = 30
			a := e.request(t, privileged.TypeKubectl, "kubectl-1", args)
			a, card := e.preflighted(t, a.ID)
			cardHas(t, card, "Run kubectl get on test-cluster", "It only reads")
			if len(k.runs) != 0 {
				t.Fatalf("a read was run before approval: %+v", k.runs)
			}
			e.approve(t, a.ID)
			e.runExecutor(t)
			if a = e.get(t, a.ID); a.State != tc.state {
				t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
			}
			if k.runs[0].Timeout != 30*time.Second {
				t.Errorf("timeout = %v", k.runs[0].Timeout)
			}
			if tc.check != nil {
				tc.check(t, kubectlResult(t, a))
			}
		})
	}
}

// A delete is previewed by the server dry run's text, is tagged destructive and expires in an hour.
func TestKubectlDeleteShowsTheDryRunText(t *testing.T) {
	e, _, k := newKubectlEnv(t, func(_ *memKube, _ privileged.KubectlRun, dry bool) privileged.KubectlOutput {
		if dry {
			return privileged.KubectlOutput{Stdout: []byte(`pod "api-1" deleted (server dry run)`)}
		}
		return privileged.KubectlOutput{Stdout: []byte(`pod "api-1" deleted`)}
	})
	a := e.request(t, privileged.TypeKubectl, "kubectl-1", kubectlArgs("delete", "pod", "api-1", "-n", appNS))
	if d := a.ExpiresAt.Sub(a.CreatedAt); d > time.Hour+time.Minute {
		t.Errorf("expires in %v", d)
	}
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, `pod "api-1" deleted (server dry run)`, "destructive: deletes objects")
	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded || k.real() != 1 {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
}

// Commands that pick another target or credential, read the executor's files, print Secrets or
// have no bounded preview are refused when requested.
func TestKubectlRequestRefusals(t *testing.T) {
	e, _, _ := newKubectlEnv(t, labelSettings)
	for i, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"get", "pods", "--kubeconfig=/tmp/x"}, "--kubeconfig is refused"},
		{[]string{"get", "pods", "--context", "prod"}, "--context is refused"},
		{[]string{"get", "pods", "--token=abc"}, "--token is refused"},
		{[]string{"get", "pods", "--as=system:admin"}, "--as is refused"},
		{[]string{"get", "pods", "-s", "https://example.test"}, "give that flag in its long form"},
		{[]string{"get", "--raw", "/api/v1/secrets"}, "--raw is refused"},
		{[]string{"get", "pods", "-v=9"}, "give that flag in its long form"},
		{[]string{"get", "pods", "--v=9"}, "--v is refused"},
		{[]string{"apply", "-f", "/etc/passwd"}, "-f takes only -"},
		{[]string{"apply", "-k", "."}, "give that flag in its long form"},
		{[]string{"apply", "--prune", "-f", "-"}, "--prune is refused"},
		{[]string{"get", "secrets", "-n", appNS}, "of Secrets is refused"},
		{[]string{"describe", "Secret/db", "-n", appNS}, "of Secrets is refused"},
		{[]string{"get", "configmaps,secrets.v1.", "-A"}, "of Secrets is refused"},
		{[]string{"wait", "--for=jsonpath={.data.password}=Z3Vlc3M=", "secret/db"}, "of Secrets is refused"},
		{[]string{"create", "secret", "generic", "db"}, "a secret comes from 1Password"},
		{[]string{"create", "token", "default"}, "kubectl create token is refused"},
		{[]string{"config", "view"}, "kubectl config is not allowed"},
		{[]string{"cp", "widgets/api-1:/etc/x", "x"}, "kubectl cp is not allowed"},
		{[]string{"edit", "deployment/api"}, "kubectl edit is not allowed"},
		{[]string{"exec", "api-1", "-n", appNS, "ls"}, "takes its command after --"},
		{[]string{"delete", "pods", "--all-namespaces", "-l", "app=widgets"}, "--all-namespaces is refused"},
		{[]string{"delete", "pods", "--all", "-n", appNS}, "--all is refused"},
		{[]string{"rollout", "undo-all"}, "kubectl rollout takes one of"},
		{[]string{"get", "pods", "-o", "go-template-file=/etc/x"}, "reads a local file"},
		{[]string{"annotate", "deploy/api", "note=password=hunter2hunter2"}, "looks like a credential"},
		{[]string{"get", "pods", "-w"}, "-w (--watch) is refused"},
	} {
		_, err := e.svc.Request(context.Background(), mainSession, privileged.TypeKubectl, jsonArgs(kubectlArgs(tc.argv...)), "r", fmt.Sprintf("r%d", i))
		if !errors.Is(err, privileged.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.argv, err, tc.want)
		}
	}
	for i, args := range []map[string]any{
		{"target": cluster, "argv": []string{"get", "pods"}},
		{"target": "", "argv": []string{"kubectl", "get", "pods"}},
		{"target": cluster, "argv": []string{"kubectl", "get", "pods"}, "timeout_seconds": 601},
		{"target": cluster, "argv": []string{"kubectl", "get", "pods"}, "stdin": "x"},
		{"target": cluster, "argv": []string{"kubectl", "apply", "-f", "-"}},
	} {
		if _, err := e.svc.Request(context.Background(), mainSession, privileged.TypeKubectl, jsonArgs(args), "r", fmt.Sprintf("s%d", i)); !errors.Is(err, privileged.ErrInvalid) {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

// Commands that reach the executor's own namespace, change a Secret, or target another cluster are
// refused at preflight, and nothing runs but the dry run.
func TestKubectlPreflightRefusals(t *testing.T) {
	secret := func(_ *memKube, _ privileged.KubectlRun, _ bool) privileged.KubectlOutput {
		return privileged.KubectlOutput{Stdout: []byte(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"widgets"},"data":{"a":"Yg=="}}`)}
	}
	ownText := func(_ *memKube, _ privileged.KubectlRun, _ bool) privileged.KubectlOutput {
		return privileged.KubectlOutput{Stdout: []byte(`validatingadmissionpolicybinding.admissionregistration.k8s.io "` + executorNS + `-guard" deleted (server dry run)`)}
	}
	for name, tc := range map[string]struct {
		args   map[string]any
		handle func(*memKube, privileged.KubectlRun, bool) privileged.KubectlOutput
		want   string
	}{
		"own namespace": {kubectlArgs("get", "pods", "-n", executorNS), labelSettings, "the executor's own namespace"},
		"own namespace in stdin": {map[string]any{"target": cluster, "argv": []string{"kubectl", "apply", "-f", "-"},
			"stdin": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n  namespace: " + executorNS + "\n"}, labelSettings, "the executor's own namespace"},
		"dry run reaches own": {kubectlArgs("delete", "validatingadmissionpolicybindings", "-l", "app=x"), ownText, "the executor's own"},
		"changes a Secret":    {kubectlArgs("label", "secret", "db", "-n", appNS, "a=b"), secret, "change a Secret are refused"},
		"another cluster":     {map[string]any{"target": "prod-02", "argv": []string{"kubectl", "get", "pods"}}, labelSettings, "not this executor's cluster"},
		"dry run fails": {kubectlArgs("scale", "deploy/api", "--replicas=2", "-n", appNS), func(*memKube, privileged.KubectlRun, bool) privileged.KubectlOutput {
			return privileged.KubectlOutput{ExitCode: 1, Stderr: []byte(`Error from server (NotFound): deployments.apps "api" not found`)}
		}, "the server dry run of kubectl scale failed (exit 1)"},
	} {
		t.Run(name, func(t *testing.T) {
			e, _, k := newKubectlEnv(t, tc.handle)
			a := e.request(t, privileged.TypeKubectl, "r1", tc.args)
			e.runExecutor(t)
			e.run(t)
			if a = e.get(t, a.ID); a.State != privileged.StatePreflightFailed || !strings.Contains(deref(a.LastError), tc.want) {
				t.Fatalf("action = %+v (%v), want %q", a, deref(a.LastError), tc.want)
			}
			if k.real() != 0 || len(e.openItems(t)) != 0 {
				t.Fatalf("runs = %+v", k.runs)
			}
		})
	}
}
