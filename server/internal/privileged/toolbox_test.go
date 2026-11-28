package privileged_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/privileged"
)

const toolboxImage = "ghcr.io/oldsj/mainloop-toolbox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeToolbox keeps the toolbox pods of command.exec in memory. next is each pod's state as the
// executor will read it: the last entry repeats.
type fakeToolbox struct {
	mu       sync.Mutex
	readyErr error
	createFn func(spec privileged.ToolboxSpec) error
	pods     map[string]*fakePod
	created  []privileged.ToolboxSpec
	deleted  []string
	outputs  int
}

type fakePod struct {
	spec    privileged.ToolboxSpec
	states  []privileged.ToolboxState
	output  string
	created time.Time
}

func newFakeToolbox() *fakeToolbox { return &fakeToolbox{pods: map[string]*fakePod{}} }

// finishes makes every new pod end with st and print output.
func (f *fakeToolbox) finishes(st privileged.ToolboxState, output string) {
	f.createFn = func(spec privileged.ToolboxSpec) error {
		f.pods[spec.ActionID] = &fakePod{spec: spec, states: []privileged.ToolboxState{st}, output: output, created: time.Now()}
		return nil
	}
}

func (f *fakeToolbox) Ready(context.Context) error { return f.readyErr }

func (f *fakeToolbox) Create(_ context.Context, spec privileged.ToolboxSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.pods[spec.ActionID]; ok {
		return nil // the same run
	}
	f.created = append(f.created, spec)
	return f.createFn(spec)
}

func (f *fakeToolbox) State(_ context.Context, id string) (privileged.ToolboxState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pods[id]
	if !ok {
		return privileged.ToolboxState{}, nil
	}
	st := p.states[0]
	if len(p.states) > 1 {
		p.states = p.states[1:]
	}
	st.Exists = true
	return st, nil
}

func (f *fakeToolbox) Output(_ context.Context, id string, max int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outputs++
	p, ok := f.pods[id]
	if !ok {
		return nil, errors.New("no pod")
	}
	return []byte(p.output)[:min(len(p.output), max)], nil
}

func (f *fakeToolbox) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	delete(f.pods, id)
	return nil
}

func (f *fakeToolbox) Runs(context.Context) ([]privileged.ToolboxRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []privileged.ToolboxRef
	for id, p := range f.pods {
		out = append(out, privileged.ToolboxRef{ActionID: id, CreatedAt: p.created})
	}
	return out, nil
}

var (
	podRan      = privileged.ToolboxState{Phase: privileged.ToolboxExited, Started: true, Reason: "Completed"}
	podRunning  = privileged.ToolboxState{Phase: privileged.ToolboxRunning, Started: true}
	podPulling  = privileged.ToolboxState{Phase: privileged.ToolboxPending, Reason: "ContainerCreating"}
	podExit3    = privileged.ToolboxState{Phase: privileged.ToolboxExited, Started: true, ExitCode: 3, Reason: "Error"}
	podTimedOut = privileged.ToolboxState{Phase: privileged.ToolboxExited, Started: true, ExitCode: 124, Reason: "Error"}
)

func newToolboxEnv(t *testing.T, f *fakeToolbox) *kubeEnv {
	t.Helper()
	e := newKubeEnv(t)
	e.svc.ToolboxOn = true
	e.ex.Toolbox, e.ex.ToolboxImage, e.ex.ToolboxPoll, e.ex.ToolboxReap = f, toolboxImage, time.Millisecond, -1
	return e
}

func execArgs(script string) map[string]any {
	return map[string]any{"target": "toolbox", "script": script, "reach": "internet"}
}

// Given a toolbox, when main requests a script, then preflight freezes the image and the reach
// and the card shows the script, stdin, the image's short digest, the timeout and the reach. After
// an Approve the executor makes one pod, follows it, returns its output and never makes a second.
func TestExecRunsAScriptInThePodOnce(t *testing.T) {
	f := newFakeToolbox()
	f.finishes(podRan, "dig +short example.org\n93.184.216.34\nHTTP/2 200\n")
	e := newToolboxEnv(t, f)
	args := execArgs("dig +short example.org\ncurl -sI https://example.org | head -1\n")
	args["stdin"] = "extra input\n"
	args["timeout_seconds"] = 90
	a := e.request(t, privileged.TypeExec, "exec-1", args)
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "Run a script in the toolbox", "dig +short example.org", "curl -sI https://example.org", "With this on its stdin", "extra input",
		"ghcr.io/oldsj/mainloop-toolbox@sha256:0123456789ab", "1m30s", "holds no credential of Mainloop's",
		"reach the internet", "not the cluster", "Nothing can preview it", "stdout and stderr together", "Risk: toolbox, reach: internet, no preview.")
	if strings.Contains(card, "0123456789abcdef0123456789abcdef") {
		t.Errorf("the card shows more than the short digest:\n%s", card)
	}
	if seen := string(privileged.AgentFacts(a.Type, a.Facts)); !strings.Contains(seen, toolboxImage) || !strings.Contains(seen, `"reach": "internet"`) {
		t.Fatalf("the agent sees %s", seen)
	}
	if len(f.created) != 0 {
		t.Fatalf("preflight made a pod: %+v", f.created)
	}
	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("after execution: %+v (%v)", a, deref(a.LastError))
	}
	if len(f.created) != 1 {
		t.Fatalf("pods made = %d", len(f.created))
	}
	spec := f.created[0]
	if spec.ActionID != a.ID || spec.Image != toolboxImage || spec.Reach != "internet" || spec.Script != args["script"] || spec.Stdin != "extra input\n" ||
		spec.Timeout != 90*time.Second {
		t.Fatalf("spec = %+v", spec)
	}
	if res := kubectlResult(t, a); res.ExitCode != 0 || !strings.Contains(res.Stdout, "93.184.216.34") {
		t.Fatalf("result = %+v", res)
	}
	e.runExecutor(t)
	if len(f.created) != 1 {
		t.Fatalf("a second pod: %d", len(f.created))
	}
}

// A script that exits non-zero fails with its output; one that ran past its timeout (the
// container's own exit 124) is uncertain; a pod whose container never started ran nothing.
func TestExecEnds(t *testing.T) {
	for name, tc := range map[string]struct {
		st     privileged.ToolboxState
		out    string
		want   string
		why    string
		stdout string
	}{
		"exit":    {podExit3, "no such host\n", privileged.StateFailed, "exited 3", "no such host"},
		"timeout": {podTimedOut, "partial\n", privileged.StateUncertain, "ran past its 1m0s and was stopped", "partial"},
		"deadline": {privileged.ToolboxState{Phase: privileged.ToolboxExited, Started: true, ExitCode: 137, Reason: "DeadlineExceeded"}, "",
			privileged.StateUncertain, "ran past its 1m0s", ""},
		"oom": {privileged.ToolboxState{Phase: privileged.ToolboxExited, Started: true, ExitCode: 137, Reason: "OOMKilled"}, "",
			privileged.StateFailed, "memory limit", ""},
		"evicted before it ran": {privileged.ToolboxState{Phase: privileged.ToolboxFailed, Reason: "Evicted"}, "",
			privileged.StateFailed, "nothing ran", ""},
		"evicted after it ran": {privileged.ToolboxState{Phase: privileged.ToolboxFailed, Started: true, Reason: "Evicted"}, "",
			privileged.StateUncertain, "may have acted", ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeToolbox()
			f.finishes(tc.st, tc.out)
			e := newToolboxEnv(t, f)
			a := e.request(t, privileged.TypeExec, "exec-1", execArgs("false"))
			a, _ = e.preflighted(t, a.ID)
			e.approve(t, a.ID)
			e.runExecutor(t)
			a = e.get(t, a.ID)
			if a.State != tc.want || !strings.Contains(deref(a.LastError), tc.why) || len(f.created) != 1 {
				t.Fatalf("%+v (%v), pods %d", a, deref(a.LastError), len(f.created))
			}
			if tc.stdout != "" && !strings.Contains(kubectlResult(t, a).Stdout, tc.stdout) {
				t.Fatalf("result = %s", a.Result)
			}
		})
	}
}

// The pod's output is redacted and bounded before it is stored.
func TestExecRedactsAndBoundsOutput(t *testing.T) {
	f := newFakeToolbox()
	f.finishes(podRan, "token ghp_"+strings.Repeat("a", 36)+"\n"+strings.Repeat("a line of ordinary output\n", 8<<10))
	e := newToolboxEnv(t, f)
	a := e.request(t, privileged.TypeExec, "exec-1", execArgs("env"))
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	e.runExecutor(t)
	a = e.get(t, a.ID)
	res := kubectlResult(t, a)
	if a.State != privileged.StateSucceeded || strings.Contains(res.Stdout, "ghp_") || len(res.Stdout) > privileged.MaxCommandOutput+200 ||
		!strings.Contains(res.Stdout, "more bytes not kept") {
		t.Fatalf("%s: stdout %d bytes: %.120s", a.State, len(res.Stdout), res.Stdout)
	}
}

// A pod that is still pending after the limit never ran anything: the run fails and its pod goes.
func TestExecPendingTooLongRanNothing(t *testing.T) {
	f := newFakeToolbox()
	f.finishes(privileged.ToolboxState{Phase: privileged.ToolboxPending, Reason: "ImagePullBackOff"}, "")
	e := newToolboxEnv(t, f)
	clock := time.Now()
	e.ex.Now = func() time.Time { clock = clock.Add(4 * time.Minute); return clock }
	a := e.request(t, privileged.TypeExec, "exec-1", execArgs("true"))
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	e.runExecutor(t)
	a = e.get(t, a.ID)
	if a.State != privileged.StateFailed || !strings.Contains(deref(a.LastError), "did not start") || !strings.Contains(deref(a.LastError), "ImagePullBackOff") ||
		!strings.Contains(deref(a.LastError), "nothing ran") || len(f.deleted) != 1 {
		t.Fatalf("%+v (%v), deleted %v", a, deref(a.LastError), f.deleted)
	}
}

// A run that waits for its pod sees it start and finish.
func TestExecFollowsThePod(t *testing.T) {
	f := newFakeToolbox()
	f.createFn = func(spec privileged.ToolboxSpec) error {
		f.pods[spec.ActionID] = &fakePod{spec: spec, states: []privileged.ToolboxState{podPulling, podPulling, podRunning, podRunning, podRan}, output: "done\n"}
		return nil
	}
	e := newToolboxEnv(t, f)
	a := e.request(t, privileged.TypeExec, "exec-1", execArgs("sleep 1"))
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded || f.outputs != 1 {
		t.Fatalf("%+v (%v), outputs read %d", a, deref(a.LastError), f.outputs)
	}
}

// If the toolbox image in the executor's configuration changed after the owner saw the card, the
// run is stale and nothing is made.
func TestExecStaleImage(t *testing.T) {
	f := newFakeToolbox()
	f.finishes(podRan, "")
	e := newToolboxEnv(t, f)
	a := e.request(t, privileged.TypeExec, "exec-1", execArgs("true"))
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	e.ex.ToolboxImage = "ghcr.io/oldsj/mainloop-toolbox@sha256:" + strings.Repeat("f", 64)
	e.runExecutor(t)
	a = e.get(t, a.ID)
	if a.State != privileged.StateStale || !strings.Contains(deref(a.LastError), "nothing ran") || len(f.created) != 0 {
		t.Fatalf("%+v (%v), pods %d", a, deref(a.LastError), len(f.created))
	}
}

// The API server refusing the pod means nothing ran. A pod made by an earlier attempt whose answer
// was lost is adopted, not made again. A started run whose pod is gone is uncertain, never rerun.
func TestExecObservesBeforeItActs(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		f := newFakeToolbox()
		f.createFn = func(privileged.ToolboxSpec) error {
			return &privileged.NothingRan{Err: errors.New("the API server refused the toolbox pod (Forbidden)")}
		}
		e := newToolboxEnv(t, f)
		a := e.request(t, privileged.TypeExec, "exec-1", execArgs("true"))
		a, _ = e.preflighted(t, a.ID)
		e.approve(t, a.ID)
		e.runExecutor(t)
		if a = e.get(t, a.ID); a.State != privileged.StateFailed || !strings.Contains(deref(a.LastError), "nothing ran") {
			t.Fatalf("%+v (%v)", a, deref(a.LastError))
		}
	})
	t.Run("lost answer, pod made", func(t *testing.T) {
		f := newFakeToolbox()
		f.createFn = func(spec privileged.ToolboxSpec) error {
			f.pods[spec.ActionID] = &fakePod{spec: spec, states: []privileged.ToolboxState{podRan}, output: "ran once\n"}
			return errors.New("connection reset")
		}
		e := newToolboxEnv(t, f)
		a := e.request(t, privileged.TypeExec, "exec-1", execArgs("true"))
		a, _ = e.preflighted(t, a.ID)
		e.approve(t, a.ID)
		e.runExecutor(t) // the create's answer is lost: retried later
		if a = e.get(t, a.ID); a.State != privileged.StateApproved && a.State != privileged.StateExecuting {
			t.Fatalf("after the lost answer: %+v", a)
		}
		e.exec(t, `UPDATE privileged_actions SET next_attempt_at = now() WHERE id = $1`, a.ID)
		e.runExecutor(t)
		if a = e.get(t, a.ID); a.State != privileged.StateSucceeded || len(f.created) != 1 || !strings.Contains(kubectlResult(t, a).Stdout, "ran once") {
			t.Fatalf("%+v (%v), pods made %d", a, deref(a.LastError), len(f.created))
		}
	})
	t.Run("lost answer, no pod", func(t *testing.T) {
		f := newFakeToolbox()
		f.createFn = func(privileged.ToolboxSpec) error { return errors.New("connection reset") }
		e := newToolboxEnv(t, f)
		a := e.request(t, privileged.TypeExec, "exec-1", execArgs("true"))
		a, _ = e.preflighted(t, a.ID)
		e.approve(t, a.ID)
		e.runExecutor(t)
		e.exec(t, `UPDATE privileged_actions SET next_attempt_at = now() WHERE id = $1`, a.ID)
		e.runExecutor(t)
		if a = e.get(t, a.ID); a.State != privileged.StateUncertain || len(f.created) != 1 || !strings.Contains(deref(a.LastError), "not run again") {
			t.Fatalf("%+v (%v), pods made %d", a, deref(a.LastError), len(f.created))
		}
	})
}

// The pod of a finished action is deleted once its outcome is stored; a pod no action knows is left
// alone while young and deleted when old.
func TestExecReapsFinishedPods(t *testing.T) {
	f := newFakeToolbox()
	f.finishes(podRan, "ok\n")
	e := newToolboxEnv(t, f)
	f.pods["pa_unknown_young"] = &fakePod{states: []privileged.ToolboxState{podRan}, created: time.Now()}
	f.pods["pa_unknown_old"] = &fakePod{states: []privileged.ToolboxState{podRan}, created: time.Now().Add(-2 * time.Hour)}
	a := e.request(t, privileged.TypeExec, "exec-1", execArgs("true"))
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	e.runExecutor(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	got := map[string]bool{}
	for _, id := range f.deleted {
		got[id] = true
	}
	if !got[a.ID] || !got["pa_unknown_old"] || got["pa_unknown_young"] || len(f.deleted) != 2 {
		t.Fatalf("deleted %v", f.deleted)
	}
}

// Without a toolbox serve-api refuses the type, and an executor without one refuses at preflight.
func TestExecNeedsAToolbox(t *testing.T) {
	e := newKubeEnv(t)
	_, err := e.svc.Request(context.Background(), mainSession, privileged.TypeExec, jsonArgs(execArgs("true")), "r", "r1")
	if !errors.Is(err, privileged.ErrUnsupported) || !strings.Contains(err.Error(), "toolbox") {
		t.Fatalf("err = %v", err)
	}
	e.svc.ToolboxOn = true
	a := e.request(t, privileged.TypeExec, "exec-1", execArgs("true"))
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != "preflight_failed" || !strings.Contains(deref(a.LastError), "no toolbox") {
		t.Fatalf("%+v (%v)", a, deref(a.LastError))
	}
	f := newFakeToolbox()
	f.readyErr = errors.New("transient")
	e.ex.Toolbox, e.ex.ToolboxImage = f, toolboxImage
	b := e.request(t, privileged.TypeExec, "exec-2", execArgs("echo hi"))
	e.runExecutor(t)
	if b = e.get(t, b.ID); b.State == privileged.StateFailed {
		t.Fatalf("a transient failure ended it: %+v (%v)", b, deref(b.LastError))
	}
}

// Arguments are validated and canonicalised: one target, one reach so far, text only, no
// credential, and nothing else in the object.
func TestExecArguments(t *testing.T) {
	e := newToolboxEnv(t, newFakeToolbox())
	bad := func(m map[string]any) map[string]any {
		out := execArgs("true")
		for k, v := range m {
			if v == nil {
				delete(out, k)
				continue
			}
			out[k] = v
		}
		return out
	}
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"target":              {bad(map[string]any{"target": "nas"}), `target is "toolbox"`},
		"both reaches":        {bad(map[string]any{"reach": "internet,cluster"}), "reach is"},
		"no reach":            {bad(map[string]any{"reach": nil}), "reach is"},
		"lan reach":           {bad(map[string]any{"reach": "lan:nas"}), "reach is"},
		"empty":               {bad(map[string]any{"script": "  \n"}), "script is the bash script"},
		"big":                 {bad(map[string]any{"script": strings.Repeat("x", 65<<10)}), "at most 64 KiB"},
		"control":             {bad(map[string]any{"script": "echo \x1b[2J"}), "no control characters"},
		"bare cr":             {bad(map[string]any{"script": "echo ok\r; rm -rf /"}), "carriage return"},
		"big stdin":           {bad(map[string]any{"stdin": strings.Repeat("x", 65<<10)}), "stdin is at most"},
		"timeout":             {bad(map[string]any{"timeout_seconds": 601}), "timeout_seconds is 1 to 600"},
		"credential":          {bad(map[string]any{"script": "curl -H 'Authorization: Bearer ghp_" + strings.Repeat("a", 36) + "' https://api.github.com"}), "contains_secret"},
		"credential in stdin": {bad(map[string]any{"stdin": "ghp_" + strings.Repeat("b", 36)}), "contains_secret"},
		"unknown field":       {bad(map[string]any{"secrets": []any{}}), "secrets"},
		"hosts for later":     {bad(map[string]any{"hosts": []any{"example.org"}}), "hosts"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.svc.Request(context.Background(), mainSession, privileged.TypeExec, jsonArgs(tc.args), "r", "req-"+strings.ReplaceAll(name, " ", "-"))
			if !errors.Is(err, privileged.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// A script naming a delete is tagged destructive and its card expires in an hour; one that
// installs software is tagged; a plain one waits a day.
func TestExecTagsAndExpiry(t *testing.T) {
	f := newFakeToolbox()
	f.finishes(podRan, "")
	e := newToolboxEnv(t, f)
	plain := e.request(t, privileged.TypeExec, "plain", execArgs("dig example.org"))
	risky := e.request(t, privileged.TypeExec, "risky", execArgs("pip install requests\ngh repo delete oldsj/scratch --yes\n"))
	if got := time.Until(plain.ExpiresAt); got < 23*time.Hour {
		t.Errorf("plain expires in %s", got)
	}
	if got := time.Until(risky.ExpiresAt); got > time.Hour+time.Minute {
		t.Errorf("destructive expires in %s", got)
	}
	e.runExecutor(t)
	e.run(t)
	for _, it := range e.openItems(t) {
		if it.QueueItem.Id == e.get(t, risky.ID).QueueItemID {
			cardHas(t, it.Title+"\n"+it.QueueItem.Content, "destructive: may delete or wipe (delete)", "may install software (pip, install)")
			return
		}
	}
	t.Fatal("no card for the risky script")
}

const proxyImage = "ghcr.io/oldsj/mainloop-backend@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

func clusterArgs(script string) map[string]any {
	a := execArgs(script)
	a["reach"] = "cluster"
	return a
}

// Cluster reach needs an executor with the proxy sidecar. With it, the card says the script reaches
// the Kubernetes API as cluster-admin through the proxy and not the internet, tags it, and shows
// the proxy's digest; the facts freeze it, and the run is stale if the proxy image changed.
func TestExecClusterReach(t *testing.T) {
	f := newFakeToolbox()
	f.finishes(podRan, "node/worker-1\n")
	e := newToolboxEnv(t, f)
	e.ex.ToolboxProxyImage = ""
	a := e.request(t, privileged.TypeExec, "exec-no-proxy", clusterArgs("kubectl get nodes"))
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != "preflight_failed" || !strings.Contains(deref(a.LastError), "no cluster reach") {
		t.Fatalf("without the proxy: %+v (%v)", a, deref(a.LastError))
	}

	e.ex.ToolboxProxyImage = proxyImage
	a = e.request(t, privileged.TypeExec, "exec-cluster", clusterArgs("kubectl get nodes\nkubectl -n widgets delete pod wedged-0\n"))
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "Run a script in the toolbox", "kubectl get nodes", "Kubernetes API as cluster-admin", "proxy sidecar (ghcr.io/oldsj/mainloop-backend@sha256:fedcba987654",
		"alone holds a token minted for this run", "refuses Secrets", "executor's and the toolbox's namespaces", "port-forward", "no internet",
		"holds the run's cluster-admin token", "destructive: may delete or wipe (delete)", "reach: cluster, cluster-admin")
	if strings.Contains(card, "can reach the internet") || strings.Contains(card, "holds no credential of Mainloop's") {
		t.Errorf("the card describes internet reach:\n%s", card)
	}
	if seen := string(privileged.AgentFacts(a.Type, a.Facts)); !strings.Contains(seen, `"proxy_image": "`+proxyImage) || !strings.Contains(seen, `"reach": "cluster"`) {
		t.Fatalf("the agent sees %s", seen)
	}
	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded || len(f.created) != 1 || f.created[0].ProxyImage != proxyImage || f.created[0].Reach != "cluster" {
		t.Fatalf("%+v (%v), pods %+v", a, deref(a.LastError), f.created)
	}

	// An internet run carries no proxy.
	b := e.request(t, privileged.TypeExec, "exec-internet", execArgs("dig example.org"))
	b, _ = e.preflighted(t, b.ID)
	if strings.Contains(string(b.Facts), "proxy_image") {
		t.Errorf("facts = %s", b.Facts)
	}

	// The proxy image changed after the owner saw the card: stale, nothing made.
	c := e.request(t, privileged.TypeExec, "exec-stale", clusterArgs("kubectl get ns"))
	c, _ = e.preflighted(t, c.ID)
	e.approve(t, c.ID)
	e.ex.ToolboxProxyImage = "ghcr.io/oldsj/mainloop-backend@sha256:" + strings.Repeat("a", 64)
	e.runExecutor(t)
	if c = e.get(t, c.ID); c.State != privileged.StateStale || len(f.created) != 1 {
		t.Fatalf("%+v (%v), pods %d", c, deref(c.LastError), len(f.created))
	}
}

// A cluster-reach pod whose proxy never produced a kubeconfig reports so in its termination
// message: the script never ran, so it fails with nothing having run.
func TestExecClusterProxyNotReady(t *testing.T) {
	f := newFakeToolbox()
	f.finishes(privileged.ToolboxState{Phase: privileged.ToolboxExited, Started: true, ExitCode: 97, Reason: "Error", Message: "kube proxy not ready"}, "")
	e := newToolboxEnv(t, f)
	e.ex.ToolboxProxyImage = proxyImage
	a := e.request(t, privileged.TypeExec, "exec-1", clusterArgs("kubectl get nodes"))
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateFailed || !strings.Contains(deref(a.LastError), "never ran") {
		t.Fatalf("%+v (%v)", a, deref(a.LastError))
	}
}

// command.exec is requestable only by a session whose Agent runs auto permission mode, locked:
// a session that is not declared so (one on a bypass Harness, an unlocked one, one whose Agent is
// unknown) is refused before anything is recorded, and the others' types are not affected.
func TestExecNeedsALockedAutoSession(t *testing.T) {
	f := newFakeToolbox()
	f.finishes(podRan, "")
	e := newToolboxEnv(t, f)
	unlocked := privileged.Requester{Role: "main", SessionID: "bypass-session"}
	_, err := e.svc.Request(context.Background(), unlocked, privileged.TypeExec, jsonArgs(execArgs("true")), "r", "r1")
	if !errors.Is(err, privileged.ErrUnsupported) || !strings.Contains(err.Error(), "auto permission mode, locked") {
		t.Fatalf("err = %v", err)
	}
	if rows := e.openItems(t); len(rows) != 0 {
		t.Fatalf("items = %d", len(rows))
	}
	var n int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM privileged_actions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows = %d (%v)", n, err)
	}
	// Another type is unaffected by the session's mode.
	if _, err := e.svc.Request(context.Background(), unlocked, privileged.TypeSSH, jsonArgs(sshArgs("true")), "r", "r2"); errors.Is(err, privileged.ErrUnsupported) &&
		strings.Contains(err.Error(), "locked") {
		t.Fatalf("command.ssh was refused for the session's mode: %v", err)
	}
	// A locked auto session is accepted.
	if _, err := e.svc.Request(context.Background(), mainSession, privileged.TypeExec, jsonArgs(execArgs("true")), "r", "r3"); err != nil {
		t.Fatal(err)
	}
}
