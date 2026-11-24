package privileged_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oldsj/mainloop/server/internal/privileged"
)

// memKube is the executor's Kubernetes access for k8s.patch and k8s.apply over an in-memory
// store: patches and applies are merge patches, a resourceVersion in the write is its
// precondition, and a dry run stores nothing.
type memKube struct {
	privileged.Kube
	objects map[string]map[string]any
	rv      int
	// conflict makes an apply without force a field conflict.
	conflict bool
	// writes counts the real (not dry-run) writes; bodies keeps the patches written.
	writes int
	bodies []string
	// lose makes the real write numbered lose succeed but answer with an error.
	lose int
	// fail makes the real write numbered fail fail without writing.
	fail int
	// reject makes the real write numbered reject refused by the API server.
	reject int
	// generate gives every created object a label the server makes up afresh on each call, as a
	// Job's controller-uid; stamp gives every write an annotation that differs on each call, as a
	// mutating webhook that stamps a time.
	generate, stamp bool
	nonce           int
	// resolveFail makes that many ResolveObject calls fail as a discovery outage would.
	resolveFail int
}

var memResources = map[string]privileged.Resource{
	"configmap":   {Version: "v1", Resource: "configmaps", Kind: "ConfigMap", Namespaced: true},
	"secret":      {Version: "v1", Resource: "secrets", Kind: "Secret", Namespaced: true},
	"namespace":   {Version: "v1", Resource: "namespaces", Kind: "Namespace"},
	"clusterrole": {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles", Kind: "ClusterRole"},
}

func newMemKube(e *kubeEnv) *memKube {
	m := &memKube{Kube: e.ex.Kube, objects: map[string]map[string]any{}, rv: 100}
	e.ex.Kube = m
	return m
}

func (m *memKube) key(r privileged.Resource, ns, name string) string {
	return r.Resource + "/" + ns + "/" + name
}

func (m *memKube) put(r privileged.Resource, obj map[string]any) {
	u := &unstructured.Unstructured{Object: obj}
	m.rv++
	u.SetResourceVersion(strconv.Itoa(m.rv))
	if u.GetUID() == "" {
		u.SetUID(types.UID("uid-" + u.GetName()))
	}
	m.objects[m.key(r, u.GetNamespace(), u.GetName())] = u.Object
}

func (m *memKube) Resolve(_ context.Context, kind string) (privileged.Resource, error) {
	name, _, _ := strings.Cut(strings.ToLower(kind), ".")
	if r, ok := memResources[strings.TrimSuffix(name, "s")]; ok {
		return r, nil
	}
	return privileged.Resource{}, fmt.Errorf("%w: the cluster has no such kind", privileged.ErrBadTarget)
}

func (m *memKube) ResolveObject(ctx context.Context, _, kind string) (privileged.Resource, error) {
	if m.resolveFail > 0 {
		m.resolveFail--
		return privileged.Resource{}, errors.New("mint a token for mainloop-executor-writer: connection refused")
	}
	return m.Resolve(ctx, kind)
}

func (m *memKube) Get(_ context.Context, r privileged.Resource, ns, name string) (map[string]any, error) {
	o, ok := m.objects[m.key(r, ns, name)]
	if !ok {
		return nil, nil
	}
	return clone(o), nil
}

func (m *memKube) Patch(_ context.Context, r privileged.Resource, ns, name string, pt types.PatchType, body []byte, dryRun bool) (map[string]any, error) {
	live, ok := m.objects[m.key(r, ns, name)]
	if !ok {
		return nil, privileged.ErrGone
	}
	raw, _ := json.Marshal(live)
	var out []byte
	var err error
	if pt == types.JSONPatchType {
		var p jsonpatch.Patch
		if p, err = jsonpatch.DecodePatch(body); err == nil {
			out, err = p.Apply(raw)
		}
	} else {
		out, err = jsonpatch.MergePatch(raw, body)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s", privileged.ErrBadTarget, err.Error())
	}
	return m.store(r, live, out, dryRun, string(body))
}

func (m *memKube) Apply(_ context.Context, r privileged.Resource, obj map[string]any, force, dryRun bool) (map[string]any, error) {
	if m.conflict && !force {
		return nil, fmt.Errorf("%w: Apply failed with 1 conflict: conflict with \"helm\": .data.mode", privileged.ErrFieldConflict)
	}
	u := &unstructured.Unstructured{Object: obj}
	live := m.objects[m.key(r, u.GetNamespace(), u.GetName())]
	base := []byte("{}")
	if live != nil {
		base, _ = json.Marshal(live)
	}
	body, _ := json.Marshal(obj)
	out, err := jsonpatch.MergePatch(base, body)
	if err != nil {
		return nil, err
	}
	return m.store(r, live, out, dryRun, string(body))
}

// store checks the write's resourceVersion against live's, then keeps it unless dryRun.
func (m *memKube) store(r privileged.Resource, live map[string]any, out []byte, dryRun bool, body string) (map[string]any, error) {
	var o map[string]any
	if err := json.Unmarshal(out, &o); err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: o}
	if m.stamp {
		m.nonce++
		_ = unstructured.SetNestedField(o, fmt.Sprintf("2026-11-24T10:%02d:00Z", m.nonce), "metadata", "annotations", "example.com/stamped-at")
	}
	if m.generate && live == nil {
		m.nonce++
		_ = unstructured.SetNestedField(o, fmt.Sprintf("generated-%d", m.nonce), "metadata", "labels", "controller-uid")
	}
	if live != nil {
		was := (&unstructured.Unstructured{Object: live}).GetResourceVersion()
		if rv := u.GetResourceVersion(); rv != "" && rv != was {
			return nil, fmt.Errorf("%w: it changed meanwhile", privileged.ErrTargetChanged)
		}
	}
	if dryRun {
		return o, nil
	}
	m.writes++
	if m.writes == m.reject {
		return nil, fmt.Errorf("%w: the API server rejects it: admission webhook \"policy.example.com\" denied the request", privileged.ErrBadTarget)
	}
	if m.writes == m.fail {
		return nil, errors.New("connection refused")
	}
	m.bodies = append(m.bodies, body)
	m.put(r, o)
	if m.writes == m.lose {
		return nil, errors.New("connection reset by peer")
	}
	return clone(m.objects[m.key(r, u.GetNamespace(), u.GetName())]), nil
}

func clone(o map[string]any) map[string]any {
	b, _ := json.Marshal(o)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func configMap(ns, name string, data map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"namespace": ns, "name": name, "labels": map[string]any{"app": "widgets"}},
		"data":     data,
	}
}

func (m *memKube) data(t *testing.T, ns, name string) map[string]any {
	t.Helper()
	o := m.objects[m.key(memResources["configmap"], ns, name)]
	if o == nil {
		t.Fatalf("no ConfigMap %s/%s", ns, name)
	}
	d, _ := o["data"].(map[string]any)
	return d
}

func setPath(m *memKube, ns, name string, value any, path ...string) {
	o := m.objects[m.key(memResources["configmap"], ns, name)]
	_ = unstructured.SetNestedField(o, value, path...)
	m.rv++
	_ = unstructured.SetNestedField(o, strconv.Itoa(m.rv), "metadata", "resourceVersion")
}

func changeResult(t *testing.T, a privileged.Action) privileged.ChangeResult {
	t.Helper()
	var res privileged.ChangeResult
	if err := json.Unmarshal(a.Result, &res); err != nil {
		t.Fatalf("result %s: %v", a.Result, err)
	}
	return res
}

var patchFast = map[string]any{
	"namespace": appNS, "kind": "configmap", "name": "settings", "patch_type": "merge", "patch": map[string]any{"data": map[string]any{"mode": "fast"}},
}

// Given a ConfigMap, when main requests a merge patch, then the card shows the dry run's diff;
// after an Approve the executor writes it at the resourceVersion it observed and marks the object
// with the action.
func TestPatchShowsTheDryRunDiff(t *testing.T) {
	e := newKubeEnv(t)
	m := newMemKube(e)
	m.put(memResources["configmap"], configMap(appNS, "settings", map[string]any{"mode": "slow", "size": "3"}))
	a := e.request(t, privileged.TypePatch, "patch-1", patchFast)
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "Patch ConfigMap widgets/settings on test-cluster", "kubectl patch --type merge", "-  mode: slow", "+  mode: fast",
		"   size: \"3\"", "uid uid-settings", "10-minute credential")
	if strings.Contains(card, "resourceVersion") || strings.Contains(card, "Risk:") {
		t.Errorf("card shows server-kept metadata or risk:\n%s", card)
	}
	if m.writes != 0 {
		t.Fatalf("preflight wrote %d times", m.writes)
	}
	// The requesting session sees the object, the diff's size and hash, never the diff: it holds
	// the object's contents, read before anyone approved.
	if seen := string(privileged.AgentFacts(a.Type, a.Facts)); strings.Contains(seen, "slow") || strings.Contains(seen, "size") ||
		!strings.Contains(seen, `"object":"ConfigMap widgets/settings"`) || strings.Contains(seen, "sha256") || strings.Contains(seen, "bytes") {
		t.Fatalf("agent facts = %s", seen)
	}

	e.approve(t, a.ID)
	setPath(m, appNS, "settings", map[string]any{"ready": true}, "status") // status churn is no change
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("after execution: %+v (%v)", a, deref(a.LastError))
	}
	if d := m.data(t, appNS, "settings"); d["mode"] != "fast" || d["size"] != "3" {
		t.Fatalf("data = %v", d)
	}
	o := m.objects[m.key(memResources["configmap"], appNS, "settings")]
	if ann, _, _ := unstructured.NestedString(o, "metadata", "annotations", privileged.AnnotationAction); ann != a.ID {
		t.Fatalf("annotation = %q", ann)
	}
	if !strings.Contains(m.bodies[0], `"resourceVersion":"102"`) {
		t.Fatalf("the write is not bound to the observed resourceVersion: %s", m.bodies[0])
	}
	if res := changeResult(t, a); len(res.Objects) != 1 || res.Objects[0].Name != "settings" || res.Objects[0].Observed {
		t.Fatalf("result = %+v", res)
	}
}

// Given an approved patch, when the object changes so the patch would now do something else, then
// the action is stale and nothing is written.
func TestPatchIsStaleWhenTheResultWouldDiffer(t *testing.T) {
	e := newKubeEnv(t)
	m := newMemKube(e)
	m.put(memResources["configmap"], configMap(appNS, "settings", map[string]any{"mode": "slow"}))
	a := e.request(t, privileged.TypePatch, "patch-1", patchFast)
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	setPath(m, appNS, "settings", "medium", "data", "mode")
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateStale || !strings.Contains(deref(a.LastError), "would now differ from the approved diff") {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
	if m.writes != 0 || m.data(t, appNS, "settings")["mode"] != "medium" {
		t.Fatalf("writes = %d, data = %v", m.writes, m.data(t, appNS, "settings"))
	}
}

// Given a JSON patch whose answer is lost, when the executor runs again, then it sees its own
// annotation and succeeds without a second write. The written patch adds the annotation map and
// the resourceVersion after the requester's operations.
func TestJSONPatchLostAnswerIsObserved(t *testing.T) {
	e := newKubeEnv(t)
	m := newMemKube(e)
	m.put(memResources["configmap"], configMap(appNS, "settings", map[string]any{"mode": "slow"}))
	args := map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "settings", "patch_type": "json",
		"patch": []any{map[string]any{"op": "replace", "path": "/data/mode", "value": "fast"}}}
	a := e.request(t, privileged.TypePatch, "patch-1", args)
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "kubectl patch --type json", "+  mode: fast")
	e.approve(t, a.ID)
	m.lose = 1
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateExecuting || m.writes != 1 {
		t.Fatalf("after the lost answer: %+v, writes %d", a, m.writes)
	}
	if b := m.bodies[0]; !strings.Contains(b, `"path":"/metadata/annotations","value":{"mainloop.dev/privileged-action":"`+a.ID+`"}`) ||
		!strings.Contains(b, `"path":"/metadata/resourceVersion","value":"101"`) {
		t.Fatalf("written patch = %s", b)
	}
	e.exec(t, `UPDATE privileged_actions SET next_attempt_at = now() WHERE id = $1`, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded || m.writes != 1 {
		t.Fatalf("after observing: %+v, writes %d", a, m.writes)
	}
	if res := changeResult(t, a); !res.Objects[0].Observed {
		t.Fatalf("result = %+v", res)
	}
}

const manifests = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
data:
  mode: fast
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: limits
  creationTimestamp: null
status: {}
data:
  max: "10"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: same
  namespace: widgets
data:
  keep: "yes"
`

func applyArgs() map[string]any {
	return map[string]any{"namespace": appNS, "manifests": manifests}
}

// Given manifests that change one ConfigMap, create another and leave a third as it is, then the
// card shows two diffs and names the third; after an Approve the executor applies the two.
func TestApplyCreatesAndUpdates(t *testing.T) {
	e := newKubeEnv(t)
	m := newMemKube(e)
	m.put(memResources["configmap"], configMap(appNS, "settings", map[string]any{"mode": "slow"}))
	same := configMap(appNS, "same", map[string]any{"keep": "yes"})
	delete(same["metadata"].(map[string]any), "labels")
	m.put(memResources["configmap"], same)
	a := e.request(t, privileged.TypeApply, "apply-1", applyArgs())
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "Apply 2 objects on test-cluster", "kubectl apply --server-side", "ConfigMap widgets/settings (uid uid-settings)",
		"+  mode: fast", "ConfigMap widgets/limits (new)", "+  max: \"10\"", "Left as they are: ConfigMap widgets/same", "creates 1 object(s)")

	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("after execution: %+v (%v)", a, deref(a.LastError))
	}
	if m.writes != 2 || m.data(t, appNS, "settings")["mode"] != "fast" || m.data(t, appNS, "limits")["max"] != "10" {
		t.Fatalf("writes %d, objects %v", m.writes, m.objects)
	}
	if !strings.Contains(m.bodies[0], `"resourceVersion":"101"`) || strings.Contains(m.bodies[1], "resourceVersion") {
		t.Fatalf("bodies = %v", m.bodies)
	}
	if res := changeResult(t, a); len(res.Objects) != 2 || !res.Objects[1].Created {
		t.Fatalf("result = %+v", res)
	}
}

// Given an apply whose second write fails after the first landed, when the executor runs again,
// then it counts the first as its own (its approved change is now no change) and writes the second.
func TestApplyRetryFinishesAPartialApply(t *testing.T) {
	e := newKubeEnv(t)
	m := newMemKube(e)
	m.put(memResources["configmap"], configMap(appNS, "settings", map[string]any{"mode": "slow"}))
	a := e.request(t, privileged.TypeApply, "apply-1", applyArgs())
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	m.fail = 2
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateExecuting {
		t.Fatalf("after the failed write: %+v (%v)", a, deref(a.LastError))
	}
	e.exec(t, `UPDATE privileged_actions SET next_attempt_at = now() WHERE id = $1`, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("after the retry: %+v (%v)", a, deref(a.LastError))
	}
	res := changeResult(t, a)
	// settings landed on the first attempt; limits and same (both new here) on the second.
	if len(res.Objects) != 3 || !res.Objects[0].Observed || res.Objects[1].Observed || m.data(t, appNS, "limits")["max"] != "10" {
		t.Fatalf("result = %+v", res)
	}
}

// Given an apply that creates an object, when someone else creates it before the first attempt,
// then the action is stale and nothing is written.
func TestApplyIsStaleWhenACreationRaced(t *testing.T) {
	e := newKubeEnv(t)
	m := newMemKube(e)
	m.put(memResources["configmap"], configMap(appNS, "settings", map[string]any{"mode": "slow"}))
	a := e.request(t, privileged.TypeApply, "apply-1", applyArgs())
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	m.put(memResources["configmap"], configMap(appNS, "limits", map[string]any{"max": "10"}))
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateStale || !strings.Contains(deref(a.LastError), "ConfigMap widgets/limits was created by someone else") {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
	if m.writes != 0 {
		t.Fatalf("writes = %d", m.writes)
	}
}

// Given requests k8s.patch and k8s.apply must not run, then each ends preflight_failed (or
// invalid) with the reason, and no card opens.
func TestChangeRefusals(t *testing.T) {
	cm := func(ns, name string) string {
		return fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: %s\ndata:\n  a: b\n", name, ns)
	}
	for name, tc := range map[string]struct {
		typ      string
		args     map[string]any
		conflict bool
		want     string
	}{
		"patch a Secret": {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "Secret", "name": "creds", "patch_type": "merge",
			"patch": map[string]any{"data": map[string]any{"a": "Yg=="}}}, false, "Secrets are refused here"},
		"apply a Secret": {privileged.TypeApply, map[string]any{"manifests": "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n  namespace: widgets\n"},
			false, "Secrets are refused here"},
		"own namespace": {privileged.TypeApply, map[string]any{"manifests": cm(executorNS, "x")}, false, "executor's own"},
		"own role": {privileged.TypePatch, map[string]any{"kind": "ClusterRole.rbac.authorization.k8s.io", "name": executorNS + "-writer",
			"patch_type": "merge", "patch": map[string]any{"rules": []any{}}}, false, "executor's own"},
		"field conflict": {privileged.TypeApply, map[string]any{"manifests": cm(appNS, "settings")}, true,
			"conflict with \"helm\": .data.mode; set force_conflicts to take them"},
		"no change": {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "settings", "patch_type": "merge",
			"patch": map[string]any{"data": map[string]any{"mode": "slow"}}}, false, "the patch changes nothing"},
		"missing": {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "nope", "patch_type": "merge",
			"patch": map[string]any{"data": map[string]any{"a": "b"}}}, false, "ConfigMap widgets/nope was not found"},
		"no namespace": {privileged.TypeApply, map[string]any{"manifests": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"},
			false, "give its namespace"},
		"cluster-scoped, ns": {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "Namespace", "name": "billing", "patch_type": "merge",
			"patch": map[string]any{"metadata": map[string]any{"labels": map[string]any{"a": "b"}}}}, false, "is cluster-scoped"},
		"over the card": {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "settings", "patch_type": "merge",
			"patch": map[string]any{"data": map[string]any{"big": strings.Repeat("x ", privileged.MaxDiffBytes/2)}}}, false, "over the 16 KiB a card shows whole"},
		"a credential": {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "settings", "patch_type": "merge",
			"patch": map[string]any{"data": map[string]any{"api_key": "hunter2hunter2"}}}, false, "a credential at /data/api_key (contains_secret)"},
		"a random key in data": {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "settings", "patch_type": "merge",
			"patch": map[string]any{"data": map[string]any{"signing": "q8Zt3vXk9LmP2wRb7YcN4hJs6FdA1eGu0TiKoQzW5xV="}}}, false, "a credential at /data/signing"},
		"a token prefix": {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "settings", "patch_type": "merge",
			"patch": map[string]any{"metadata": map[string]any{"annotations": map[string]any{"example.com/note": "ghp_exampleexampleexample0000"}}}},
			false, "a credential at /metadata/annotations/example.com~1note"},
		"unknown kind": {privileged.TypeApply, map[string]any{"manifests": "apiVersion: example.com/v1\nkind: Widget\nmetadata:\n  name: w\n"},
			false, "the cluster has no such kind"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newKubeEnv(t)
			m := newMemKube(e)
			m.conflict = tc.conflict
			m.put(memResources["configmap"], configMap(appNS, "settings", map[string]any{"mode": "slow"}))
			a := e.request(t, tc.typ, "r1", tc.args)
			e.runExecutor(t)
			e.run(t)
			if a = e.get(t, a.ID); a.State != privileged.StatePreflightFailed || !strings.Contains(deref(a.LastError), tc.want) {
				t.Fatalf("action = %+v (%v), want %q", a, deref(a.LastError), tc.want)
			}
			if n := len(e.openItems(t)); n != 0 || m.writes != 0 {
				t.Fatalf("items = %d, writes = %d", n, m.writes)
			}
		})
	}
	merge := func(patch any) map[string]any {
		return map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "x", "patch_type": "merge", "patch": patch}
	}
	for name, tc := range map[string]struct {
		typ  string
		args map[string]any
	}{
		"bad patch type":     {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "x", "patch_type": "apply", "patch": map[string]any{"a": 1}}},
		"empty patch":        {privileged.TypePatch, merge(map[string]any{})},
		"array merge patch":  {privileged.TypePatch, merge([]any{1})},
		"sets the rv":        {privileged.TypePatch, merge(map[string]any{"metadata": map[string]any{"resourceVersion": "1"}})},
		"sets status":        {privileged.TypePatch, merge(map[string]any{"metadata": map[string]any{"labels": map[string]any{"a": "b"}}, "status": map[string]any{"phase": "Approved"}})},
		"sets managedFields": {privileged.TypePatch, merge(map[string]any{"metadata": map[string]any{"managedFields": []any{}}})},
		"null annotations":   {privileged.TypePatch, merge(map[string]any{"metadata": map[string]any{"annotations": nil}})},
		"null metadata":      {privileged.TypePatch, merge(map[string]any{"metadata": nil})},
		"json patch status": {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "x", "patch_type": "json",
			"patch": []any{map[string]any{"op": "add", "path": "/status/phase", "value": "Approved"}}}},
		"json patch managedFields": {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "x", "patch_type": "json",
			"patch": []any{map[string]any{"op": "remove", "path": "/metadata/managedFields"}}}},
		"manifest with status": {privileged.TypeApply, map[string]any{"manifests": cm(appNS, "x") + "status:\n  phase: Approved\n"}},
		"sets the action":      {privileged.TypePatch, merge(map[string]any{"metadata": map[string]any{"annotations": map[string]any{privileged.AnnotationAction: "x"}}})},
		"json patch the rv": {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "x", "patch_type": "json",
			"patch": []any{map[string]any{"op": "replace", "path": "/metadata/resourceVersion", "value": "1"}}}},
		"json patch the metadata": {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "x", "patch_type": "json",
			"patch": []any{map[string]any{"op": "remove", "path": "/metadata"}}}},
		"json patch test op": {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "x", "patch_type": "json",
			"patch": []any{map[string]any{"op": "test", "path": "/data/mode", "value": "slow"}}}},
		"json patch bad op": {privileged.TypePatch, map[string]any{"namespace": appNS, "kind": "ConfigMap", "name": "x", "patch_type": "json",
			"patch": []any{map[string]any{"op": "exec", "path": "/data"}}}},
		"no manifests":        {privileged.TypeApply, map[string]any{"manifests": " "}},
		"a List":              {privileged.TypeApply, map[string]any{"manifests": "apiVersion: v1\nkind: ConfigMapList\nitems: []\n"}},
		"generateName":        {privileged.TypeApply, map[string]any{"manifests": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  generateName: x-\n"}},
		"twice":               {privileged.TypeApply, map[string]any{"manifests": cm(appNS, "x") + "---\n" + cm(appNS, "x")}},
		"other namespace":     {privileged.TypeApply, map[string]any{"namespace": "billing", "manifests": cm(appNS, "x")}},
		"manifest sets rv":    {privileged.TypeApply, map[string]any{"manifests": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n  resourceVersion: \"5\"\n"}},
		"not yaml":            {privileged.TypeApply, map[string]any{"manifests": "kind: [unclosed"}},
		"manifests too large": {privileged.TypeApply, map[string]any{"manifests": cm(appNS, "x") + "# " + strings.Repeat("x", privileged.MaxChangeBytes)}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newKubeEnv(t)
			_, err := e.svc.Request(context.Background(), mainSession, tc.typ, jsonArgs(tc.args), "r", "r1")
			if !errors.Is(err, privileged.ErrInvalid) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// Given writes the API server stamps with values it makes up on each call (a Job's controller-uid
// on creation, a webhook's timestamp on every write), then the card shows them as generated and
// the approved change runs rather than going stale on them.
func TestServerGeneratedValuesAreMasked(t *testing.T) {
	t.Run("apply", func(t *testing.T) {
		e := newKubeEnv(t)
		m := newMemKube(e)
		m.generate, m.stamp = true, true
		m.put(memResources["configmap"], configMap(appNS, "settings", map[string]any{"mode": "slow"}))
		a := e.request(t, privileged.TypeApply, "apply-1", applyArgs())
		a, card := e.preflighted(t, a.ID)
		cardHas(t, card, "controller-uid: <server-generated>", "example.com/stamped-at: <server-generated>",
			"<server-generated> marks values the API server generates on each run (/metadata/annotations/example.com~1stamped-at, /metadata/labels/controller-uid)")
		e.approve(t, a.ID)
		e.runExecutor(t)
		if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
			t.Fatalf("after execution: %+v (%v)", a, deref(a.LastError))
		}
	})
	t.Run("patch", func(t *testing.T) {
		e := newKubeEnv(t)
		m := newMemKube(e)
		m.stamp = true
		m.put(memResources["configmap"], configMap(appNS, "settings", map[string]any{"mode": "slow"}))
		a := e.request(t, privileged.TypePatch, "patch-1", patchFast)
		a, card := e.preflighted(t, a.ID)
		cardHas(t, card, "+    example.com/stamped-at: <server-generated>")
		e.approve(t, a.ID)
		e.runExecutor(t)
		if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
			t.Fatalf("after execution: %+v (%v)", a, deref(a.LastError))
		}
	})
}

// Given an approved apply, when discovery or the token mint fails once at run time, then the
// executor retries rather than ending the approval stale.
func TestApplyRetriesATransientFailure(t *testing.T) {
	e := newKubeEnv(t)
	m := newMemKube(e)
	m.put(memResources["configmap"], configMap(appNS, "settings", map[string]any{"mode": "slow"}))
	a := e.request(t, privileged.TypeApply, "apply-1", applyArgs())
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	m.resolveFail = 1
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateExecuting || !strings.Contains(deref(a.LastError), "connection refused") {
		t.Fatalf("after the outage: %+v (%v)", a, deref(a.LastError))
	}
	e.exec(t, `UPDATE privileged_actions SET next_attempt_at = now() WHERE id = $1`, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("after the retry: %+v (%v)", a, deref(a.LastError))
	}
}

// Given an apply whose second write the API server refuses, then the action fails at once, and
// its result says the first object was written.
func TestApplyRejectedWriteKeepsWhatWasWritten(t *testing.T) {
	e := newKubeEnv(t)
	m := newMemKube(e)
	m.put(memResources["configmap"], configMap(appNS, "settings", map[string]any{"mode": "slow"}))
	a := e.request(t, privileged.TypeApply, "apply-1", applyArgs())
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	m.reject = 2
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateFailed || !strings.Contains(deref(a.LastError), "ConfigMap widgets/limits") ||
		!strings.Contains(deref(a.LastError), "denied the request") {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
	if res := changeResult(t, a); len(res.Objects) != 1 || res.Objects[0].Name != "settings" {
		t.Fatalf("result = %+v", res)
	}
}

// Given manifests with what a credential check that takes every long word for a token would refuse
// (a 41-character name, a checksum annotation, a CA certificate, a hex digest in data), then the
// card opens.
func TestOrdinaryLongValuesAreNotCredentials(t *testing.T) {
	e := newKubeEnv(t)
	newMemKube(e)
	const manifest = `apiVersion: v1
kind: ConfigMap
metadata:
  name: kube-prometheus-stack-prometheus-operator
  annotations:
    checksum/config: 6f1ed002ab5595859014ebf0951522d9ee1a5fe0ac0f0a5e8e8f1e5d0c0b1a2f
    example.com/ca-bundle: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUJrVENCK3dJSkFQRXhhbXBsZQo=
data:
  digest: 6f1ed002ab5595859014ebf0951522d9ee1a5fe0ac0f0a5e8e8f1e5d0c0b1a2f
  ca.crt: |
    -----BEGIN CERTIFICATE-----
    MIIBkTCB+wIJAPExampleExampleExampleExampleExample
    -----END CERTIFICATE-----
`
	a := e.request(t, privileged.TypeApply, "apply-1", map[string]any{"namespace": appNS, "manifests": manifest})
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "kube-prometheus-stack-prometheus-operator", "checksum/config")
	if a.State != privileged.StatePendingApproval {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
}

// Given a manifest that sets a value the API server makes up afresh on every write, then it is
// refused rather than masked: the owner would not see what is written there.
func TestGeneratedValueTheRequestSetsIsRefused(t *testing.T) {
	e := newKubeEnv(t)
	m := newMemKube(e)
	m.generate = true
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: batch\n  labels:\n    controller-uid: mine\ndata:\n  a: b\n"
	a := e.request(t, privileged.TypeApply, "apply-1", map[string]any{"namespace": appNS, "manifests": manifest})
	e.runExecutor(t)
	e.run(t)
	if a = e.get(t, a.ID); a.State != privileged.StatePreflightFailed ||
		!strings.Contains(deref(a.LastError), "the value at /metadata/labels/controller-uid, which the request sets, came out differently") {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
}
