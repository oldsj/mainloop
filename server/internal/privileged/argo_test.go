package privileged_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/oldsj/mainloop/server/internal/privileged"
)

const syncedSHA = "0123456789abcdef0123456789abcdef01234567"

var appsGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// argoApp is an auto-synced Application with one Deployment out of sync and one ConfigMap to prune.
func argoApp(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"namespace": "argocd", "name": name, "uid": "app-uid-1", "resourceVersion": "7"},
		"spec": map[string]any{
			"syncPolicy":  map[string]any{"automated": map[string]any{}},
			"destination": map[string]any{"namespace": appNS, "server": "https://kubernetes.default.svc"},
		},
		"status": map[string]any{
			"sync":   map[string]any{"status": "OutOfSync", "revision": syncedSHA},
			"health": map[string]any{"status": "Healthy"},
			"resources": []any{
				map[string]any{"group": "apps", "kind": "Deployment", "namespace": appNS, "name": "api", "status": "OutOfSync"},
				map[string]any{"kind": "ConfigMap", "namespace": appNS, "name": "old", "status": "OutOfSync", "requiresPruning": true},
				map[string]any{"kind": "Service", "namespace": appNS, "name": "api", "status": "Synced"},
			},
		},
	}}
}

type argoEnv struct {
	*kubeEnv
	dyn *dynamicfake.FakeDynamicClient
}

func newArgoEnv(t *testing.T, apps ...*unstructured.Unstructured) *argoEnv {
	t.Helper()
	e := &argoEnv{kubeEnv: newKubeEnv(t)}
	objs := make([]runtime.Object, 0, len(apps))
	for _, a := range apps {
		objs = append(objs, a)
	}
	e.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{appsGVR: "ApplicationList"}, objs...)
	k := e.ex.Kube.(privileged.KubeActions)
	k.Argo = func(context.Context) (dynamic.Interface, error) { return e.dyn, nil }
	e.ex.Kube = k
	return e
}

func (e *argoEnv) app(t *testing.T, name string) map[string]any {
	t.Helper()
	o, err := e.dyn.Resource(appsGVR).Namespace("argocd").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return o.Object
}

func (e *argoEnv) setApp(t *testing.T, name string, value any, path ...string) {
	t.Helper()
	o := &unstructured.Unstructured{Object: e.app(t, name)}
	if err := unstructured.SetNestedField(o.Object, value, path...); err != nil {
		t.Fatal(err)
	}
	if _, err := e.dyn.Resource(appsGVR).Namespace("argocd").Update(context.Background(), o, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// Given an auto-synced Application out of sync, when main requests a sync with prune, then the
// card names the revision, the resources out of sync and the prune; after an Approve the executor
// writes the operation pinned to that revision, marked with the action.
func TestArgoSyncStartsASyncAtTheSeenRevision(t *testing.T) {
	e := newArgoEnv(t, argoApp("widgets"))
	a := e.request(t, privileged.TypeArgoSync, "sync-1", map[string]any{"app": "widgets", "prune": true})
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "Sync Argo CD app widgets on test-cluster with prune", "revision 0123456789ab", "OutOfSync, healthy",
		"It deploys to namespace widgets on https://kubernetes.default.svc.", "Out of sync (2):", "- ConfigMap widgets/old (to prune)", "- Deployment.apps widgets/api", "destructive: prunes resources", "auto-sync is on")
	if strings.Contains(card, "Service") {
		t.Errorf("the card lists a synced resource:\n%s", card)
	}
	// Pruning deletes, so a sync with prune waits an hour for the owner, as a delete does.
	if until := time.Until(a.ExpiresAt); until > time.Hour || until < 55*time.Minute {
		t.Fatalf("expires in %s, want an hour", until)
	}

	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("after execution: %+v (%v)", a, deref(a.LastError))
	}
	app := e.app(t, "widgets")
	rev, _, _ := unstructured.NestedString(app, "operation", "sync", "revision")
	prune, _, _ := unstructured.NestedBool(app, "operation", "sync", "prune")
	by, _, _ := unstructured.NestedString(app, "operation", "initiatedBy", "username")
	if rev != syncedSHA || !prune || by != "mainloop/"+a.ID || privileged.ParseArgoApp(app).Action != a.ID {
		t.Fatalf("operation = %v", app["operation"])
	}
	var res privileged.ArgoResult
	if err := json.Unmarshal(a.Result, &res); err != nil || res.App != "widgets" || res.Revisions[0] != syncedSHA || res.Observed {
		t.Fatalf("result = %s", a.Result)
	}
}

// Given an approved sync, when Git moves before it runs, then it is stale and nothing starts.
func TestArgoSyncIsStaleWhenGitMoved(t *testing.T) {
	e := newArgoEnv(t, argoApp("widgets"))
	a := e.request(t, privileged.TypeArgoSync, "sync-1", map[string]any{"app": "widgets"})
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	e.setApp(t, "widgets", "fedcba9876543210fedcba9876543210fedcba98", "status", "sync", "revision")
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateStale || !strings.Contains(deref(a.LastError), "Git moved") {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
	if _, ok := e.app(t, "widgets")["operation"]; ok {
		t.Fatal("a sync started")
	}
}

// Given an approved sync, when Argo CD is running another sync, then the executor waits and starts
// it once that one ends.
func TestArgoSyncWaitsForARunningSync(t *testing.T) {
	e := newArgoEnv(t, argoApp("widgets"))
	a := e.request(t, privileged.TypeArgoSync, "sync-1", map[string]any{"app": "widgets"})
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	e.setApp(t, "widgets", "Running", "status", "operationState", "phase")
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateExecuting || !strings.Contains(deref(a.LastError), "a sync is running") {
		t.Fatalf("while running: %+v (%v)", a, deref(a.LastError))
	}
	e.setApp(t, "widgets", "Succeeded", "status", "operationState", "phase")
	e.exec(t, `UPDATE privileged_actions SET next_attempt_at = now() WHERE id = $1`, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("after it ended: %+v (%v)", a, deref(a.LastError))
	}
}

// A multi-source Application's sync pins every source's revision.
func TestSyncPatchPinsEachSource(t *testing.T) {
	o := argoApp("widgets").Object
	_ = unstructured.SetNestedStringSlice(o, []string{syncedSHA, "v1.2.3"}, "status", "sync", "revisions")
	app := privileged.ParseArgoApp(o)
	body, err := privileged.SyncPatch(app, false, "act-1")
	if err != nil {
		t.Fatal(err)
	}
	if s := string(body); !strings.Contains(s, `"revisions":["`+syncedSHA+`","v1.2.3"]`) || strings.Contains(s, `"revision":`) ||
		!strings.Contains(s, `"resourceVersion":"7"`) {
		t.Fatalf("patch = %s", s)
	}
}

// Given syncs the executor must not start, then each ends preflight_failed (or invalid).
func TestArgoSyncRefusals(t *testing.T) {
	running := argoApp("busy")
	_ = unstructured.SetNestedField(running.Object, "Running", "status", "operationState", "phase")
	own := argoApp("executor")
	_ = unstructured.SetNestedField(own.Object, executorNS, "spec", "destination", "namespace")
	unresolved := argoApp("new")
	unstructured.RemoveNestedField(unresolved.Object, "status", "sync", "revision")
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"missing":       {map[string]any{"app": "nope"}, "Application argocd/nope was not found"},
		"running":       {map[string]any{"app": "busy"}, "already has a sync running"},
		"unresolved":    {map[string]any{"app": "new"}, "no resolved revision yet"},
		"own namespace": {map[string]any{"app": "x", "namespace": executorNS}, "executor's own"},
		"deploys own":   {map[string]any{"app": "executor"}, "deploys the executor's own namespace"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newArgoEnv(t, running, unresolved, own)
			a := e.request(t, privileged.TypeArgoSync, "r1", tc.args)
			e.runExecutor(t)
			e.run(t)
			if a = e.get(t, a.ID); a.State != privileged.StatePreflightFailed || !strings.Contains(deref(a.LastError), tc.want) {
				t.Fatalf("action = %+v (%v), want %q", a, deref(a.LastError), tc.want)
			}
		})
	}
	for _, args := range []map[string]any{{"app": "Bad_Name"}, {"app": "x", "namespace": "a/b"}, {"app": "x", "revision": "main"}} {
		e := newArgoEnv(t)
		if _, err := e.svc.Request(context.Background(), mainSession, privileged.TypeArgoSync, jsonArgs(args), "r", "r1"); !errors.Is(err, privileged.ErrInvalid) {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
}

// Given a sync without prune, then it waits the usual 24 hours for the owner.
func TestArgoSyncWithoutPruneExpiresInADay(t *testing.T) {
	e := newArgoEnv(t, argoApp("widgets"))
	a := e.request(t, privileged.TypeArgoSync, "sync-1", map[string]any{"app": "widgets"})
	if until := time.Until(a.ExpiresAt); until < 23*time.Hour {
		t.Fatalf("expires in %s, want a day", until)
	}
}
