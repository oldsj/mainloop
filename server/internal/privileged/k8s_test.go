package privileged_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	metadatafake "k8s.io/client-go/metadata/fake"
	"k8s.io/client-go/restmapper"

	"github.com/oldsj/mainloop/server/internal/privileged"
)

const (
	executorNS = "mainloop-executor"
	cluster    = "test-cluster"
)

var (
	podsGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
)

type kubeEnv struct {
	*execEnv
	scheme *runtime.Scheme
	meta   *metadatafake.FakeMetadataClient
}

// newKubeEnv is newExecEnv with the executor's Kubernetes access: workloads through the fake
// clientset, deletes through a fake metadata client over pods and namespaces.
func newKubeEnv(t *testing.T, objects ...*metav1.PartialObjectMetadata) *kubeEnv {
	t.Helper()
	e := &kubeEnv{execEnv: newExecEnv(t)}
	e.svc.KubernetesOn = true
	e.scheme = metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(e.scheme); err != nil {
		t.Fatal(err)
	}
	e.meta = metadatafake.NewSimpleMetadataClient(e.scheme, toRuntime(objects)...)
	mapper := restmapper.NewDiscoveryRESTMapper([]*restmapper.APIGroupResources{{
		Group: metav1.APIGroup{Versions: []metav1.GroupVersionForDiscovery{{Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{Version: "v1"}},
		VersionedResources: map[string][]metav1.APIResource{"v1": {
			{Name: "pods", SingularName: "pod", Namespaced: true, Kind: "Pod"},
			{Name: "namespaces", SingularName: "namespace", Kind: "Namespace"},
		}},
	}})
	e.ex.Kube = privileged.KubeActions{
		Workloads: func(context.Context) (kubernetes.Interface, error) { return e.kube, nil },
		Deleter:   func(context.Context) (metadata.Interface, meta.RESTMapper, error) { return e.meta, mapper, nil },
	}
	e.ex.ExecutorNamespace, e.ex.Cluster = executorNS, cluster
	return e
}

func toRuntime(objects []*metav1.PartialObjectMetadata) []runtime.Object {
	out := make([]runtime.Object, 0, len(objects))
	for _, o := range objects {
		out = append(out, o)
	}
	return out
}

func pod(ns, name, uid string, owner string) *metav1.PartialObjectMetadata {
	o := &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid), Finalizers: []string{}},
	}
	if owner != "" {
		yes := true
		o.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: owner, Controller: &yes}}
	}
	return o
}

func deployment(ns, name string, replicas int32, generation int64) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: "uid-" + types.UID(name), Generation: generation, ResourceVersion: "41"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "ghcr.io/octo-org/widgets:1.2.3"}}}},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: replicas, UpdatedReplicas: replicas},
	}
}

func (e *kubeEnv) addDeployment(t *testing.T, d *appsv1.Deployment) {
	t.Helper()
	if _, err := e.kube.AppsV1().Deployments(d.Namespace).Create(context.Background(), d, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func (e *kubeEnv) deployment(t *testing.T, ns, name string) *appsv1.Deployment {
	t.Helper()
	d, err := e.kube.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func jsonArgs(v map[string]any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (e *kubeEnv) request(t *testing.T, typ, requestID string, args map[string]any) privileged.Action {
	t.Helper()
	a, err := e.svc.Request(context.Background(), mainSession, typ, jsonArgs(args), "The widgets API is wedged after a config change.", requestID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// preflighted runs serve-executor's preflight and serve-api's card, and returns the action and the
// card's title and text.
func (e *kubeEnv) preflighted(t *testing.T, id string) (privileged.Action, string) {
	t.Helper()
	e.runExecutor(t)
	e.run(t)
	a := e.get(t, id)
	for _, it := range e.openItems(t) {
		if it.QueueItem.Id == a.QueueItemID {
			return a, it.Title + "\n" + it.QueueItem.Content
		}
	}
	t.Fatalf("no card for %+v (%v)", a, deref(a.LastError))
	return a, ""
}

func cardHas(t *testing.T, card string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(card, w) {
			t.Errorf("card lacks %q:\n%s", w, card)
		}
	}
}

// Given a Deployment, when main requests a restart, then serve-executor preflights it and the card
// names the cluster, the replicas and the images; after an Approve with the digest the executor
// sets the template's restartedAt to the approval time and annotates the Deployment with the action.
func TestRolloutRestartRunsInTheExecutor(t *testing.T) {
	e := newKubeEnv(t)
	e.addDeployment(t, deployment(appNS, "api", 3, 4))
	a := e.request(t, privileged.TypeRolloutRestart, "restart-1", map[string]any{"namespace": appNS, "kind": "deployment", "name": "api"})
	if a.State != privileged.StateRequested {
		t.Fatalf("request = %+v", a)
	}
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "Restart Deployment widgets/api on test-cluster", "kubectl rollout restart",
		"Now: 3 replicas, 3 ready, 3 updated. Images: ghcr.io/octo-org/widgets:1.2.3.", "generation 4", "10-minute credential")
	if strings.Contains(card, "Risk:") {
		t.Errorf("unexpected risk tags:\n%s", card)
	}

	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("after execution: %+v (%v)", a, deref(a.LastError))
	}
	d := e.deployment(t, appNS, "api")
	var res privileged.WorkloadResult
	if err := json.Unmarshal(a.Result, &res); err != nil {
		t.Fatal(err)
	}
	if at := d.Spec.Template.Annotations[privileged.AnnotationRestartedAt]; at == "" || at != res.RestartedAt ||
		d.Annotations[privileged.AnnotationAction] != a.ID || *d.Spec.Replicas != 3 {
		t.Fatalf("deployment = %+v, result %+v", d.ObjectMeta, res)
	}
	if _, err := time.Parse(time.RFC3339, res.RestartedAt); err != nil {
		t.Fatalf("restarted_at = %q", res.RestartedAt)
	}
}

// Given serve-api without the executor's Kubernetes access, when a k8s.* type is requested, then it
// is unsupported.
func TestKubernetesTypesNeedTheExecutorsAccess(t *testing.T) {
	e := newKubeEnv(t)
	e.svc.KubernetesOn = false
	_, err := e.svc.Request(context.Background(), mainSession, privileged.TypeScale,
		jsonArgs(map[string]any{"namespace": appNS, "kind": "Deployment", "name": "api", "replicas": 2}), "r", "r1")
	if !errors.Is(err, privileged.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

// Given a Deployment with an autoscaler, when main requests a scale to zero, then the card tags it
// destructive and names the autoscaler; and when its spec changes after the owner approved, then
// the action is stale and the replicas are left as they are.
func TestScaleIsStaleWhenTheSpecChanged(t *testing.T) {
	e := newKubeEnv(t)
	e.addDeployment(t, deployment(appNS, "api", 3, 4))
	if _, err := e.kube.AutoscalingV2().HorizontalPodAutoscalers(appNS).Create(context.Background(), &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: appNS, Name: "api-hpa"},
		Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api"}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	a := e.request(t, privileged.TypeScale, "scale-1", map[string]any{"namespace": appNS, "kind": "Deployment", "name": "api", "replicas": 0})
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "Scale Deployment widgets/api to 0 on test-cluster", "from 3 to 0", "destructive: scales to zero",
		"HorizontalPodAutoscaler api-hpa manages its replicas")

	e.approve(t, a.ID)
	d := e.deployment(t, appNS, "api")
	d.Generation = 5
	if _, err := e.kube.AppsV1().Deployments(appNS).Update(context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateStale || !strings.Contains(deref(a.LastError), "generation 4, now 5") {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
	if d = e.deployment(t, appNS, "api"); *d.Spec.Replicas != 3 || d.Annotations[privileged.AnnotationAction] != "" {
		t.Fatalf("deployment was written: %+v", d)
	}
}

// Given a scale whose status changed after approval (a new resourceVersion, the same generation),
// when the executor runs it, then the scale is written.
func TestScaleIgnoresStatusChanges(t *testing.T) {
	e := newKubeEnv(t)
	e.addDeployment(t, deployment(appNS, "api", 3, 4))
	a := e.request(t, privileged.TypeScale, "scale-1", map[string]any{"namespace": appNS, "kind": "deployments", "name": "api", "replicas": 5})
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	d := e.deployment(t, appNS, "api")
	d.Status.ReadyReplicas = 2
	d.ResourceVersion = "57"
	if _, err := e.kube.AppsV1().Deployments(appNS).UpdateStatus(context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
	if d = e.deployment(t, appNS, "api"); *d.Spec.Replicas != 5 || d.Annotations[privileged.AnnotationAction] != a.ID {
		t.Fatalf("deployment = %+v", d)
	}
}

// Given a pod its ReplicaSet controls, when main requests a delete, then the card tags it
// destructive and names the owner, it expires in an hour, and after approval the executor deletes
// that pod only.
func TestDeleteRunsWithItsUID(t *testing.T) {
	e := newKubeEnv(t, pod(appNS, "api-7d9f-x2k4", "pod-uid-1", "api-7d9f"), pod(appNS, "api-7d9f-q8r1", "pod-uid-2", "api-7d9f"))
	a := e.request(t, privileged.TypeDelete, "delete-1", map[string]any{"namespace": appNS, "kind": "Pod", "name": "api-7d9f-x2k4"})
	if until := time.Until(a.ExpiresAt); until > time.Hour || until < 55*time.Minute {
		t.Fatalf("expires in %s, want an hour", until)
	}
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "Delete Pod widgets/api-7d9f-x2k4 on test-cluster", "uid pod-uid-1", "Owners: ReplicaSet/api-7d9f.",
		"destructive, irreversible", "owned by ReplicaSet/api-7d9f, which may recreate it")

	e.approve(t, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
	if _, err := e.meta.Resource(podsGVR).Namespace(appNS).Get(context.Background(), "api-7d9f-x2k4", metav1.GetOptions{}); err == nil {
		t.Fatal("the pod is still there")
	}
	if _, err := e.meta.Resource(podsGVR).Namespace(appNS).Get(context.Background(), "api-7d9f-q8r1", metav1.GetOptions{}); err != nil {
		t.Fatalf("the other pod: %v", err)
	}
}

// Given an approved delete whose object was replaced by a new one with the same name, when the
// executor runs it, then it is stale and the new object stays.
func TestDeleteOfAReplacedObjectIsStale(t *testing.T) {
	e := newKubeEnv(t, pod(appNS, "worker", "pod-uid-1", ""))
	a := e.request(t, privileged.TypeDelete, "delete-1", map[string]any{"namespace": appNS, "kind": "pods", "name": "worker"})
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	ctx := context.Background()
	if err := e.meta.Resource(podsGVR).Namespace(appNS).Delete(ctx, "worker", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	e.meta = metadatafake.NewSimpleMetadataClient(e.scheme, toRuntime([]*metav1.PartialObjectMetadata{pod(appNS, "worker", "pod-uid-2", "")})...)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateStale || !strings.Contains(deref(a.LastError), "replaced") {
		t.Fatalf("action = %+v (%v)", a, deref(a.LastError))
	}
	if _, err := e.meta.Resource(podsGVR).Namespace(appNS).Get(ctx, "worker", metav1.GetOptions{}); err != nil {
		t.Fatalf("the new pod: %v", err)
	}
}

// Given a delete that landed but whose answer was lost, when the next attempt runs, then it
// observes the object is gone and succeeds without deleting again.
func TestDeleteLostAnswerIsObserved(t *testing.T) {
	e := newKubeEnv(t, pod(appNS, "worker", "pod-uid-1", ""))
	a := e.request(t, privileged.TypeDelete, "delete-1", map[string]any{"namespace": appNS, "kind": "Pod", "name": "worker"})
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	lost := &lostDelete{Kube: e.ex.Kube}
	e.ex.Kube = lost
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateExecuting || lost.deletes != 1 {
		t.Fatalf("after the lost answer: %+v, deletes %d", a, lost.deletes)
	}
	e.exec(t, `UPDATE privileged_actions SET next_attempt_at = now() WHERE id = $1`, a.ID)
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateSucceeded || lost.deletes != 1 {
		t.Fatalf("after observing: %+v, deletes %d", a, lost.deletes)
	}
	var res privileged.DeleteResult
	if err := json.Unmarshal(a.Result, &res); err != nil || !res.Observed || res.UID != "pod-uid-1" {
		t.Fatalf("result = %s", a.Result)
	}
}

// Given requests the executor must not run, then each ends preflight_failed (or invalid) with the
// reason, and no card opens.
func TestKubernetesRefusals(t *testing.T) {
	ns := func(name string) *metav1.PartialObjectMetadata {
		return &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: name, UID: "ns-" + types.UID(name)}}
	}
	terminating := pod(appNS, "leaving", "pod-uid-9", "")
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	terminating.Finalizers = []string{"example.com/drain"}
	for name, tc := range map[string]struct {
		typ  string
		args map[string]any
		want string
	}{
		"own namespace":        {privileged.TypeRolloutRestart, map[string]any{"namespace": executorNS, "kind": "Deployment", "name": "mainloop-executor"}, "executor's own"},
		"own namespace delete": {privileged.TypeDelete, map[string]any{"kind": "Namespace", "name": executorNS}, "executor's own"},
		"missing workload":     {privileged.TypeRolloutRestart, map[string]any{"namespace": appNS, "kind": "StatefulSet", "name": "db"}, "StatefulSet widgets/db was not found"},
		"same replicas":        {privileged.TypeScale, map[string]any{"namespace": appNS, "kind": "Deployment", "name": "api", "replicas": 3}, "already has 3 replicas"},
		"unknown kind":         {privileged.TypeDelete, map[string]any{"namespace": appNS, "kind": "Widget.example.com", "name": "w"}, "the cluster has no such kind"},
		"namespaced, no ns":    {privileged.TypeDelete, map[string]any{"kind": "Pod", "name": "worker"}, "is namespaced"},
		"cluster-scoped, ns":   {privileged.TypeDelete, map[string]any{"namespace": appNS, "kind": "Namespace", "name": "billing"}, "is cluster-scoped"},
		"missing object":       {privileged.TypeDelete, map[string]any{"namespace": appNS, "kind": "Pod", "name": "nope"}, "Pod widgets/nope was not found"},
		"terminating":          {privileged.TypeDelete, map[string]any{"namespace": appNS, "kind": "Pod", "name": "leaving"}, "already being deleted (finalizers: example.com/drain)"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newKubeEnv(t, ns(executorNS), ns("billing"), terminating)
			e.addDeployment(t, deployment(appNS, "api", 3, 4))
			e.addDeployment(t, deployment(executorNS, "mainloop-executor", 1, 1))
			a := e.request(t, tc.typ, "r1", tc.args)
			e.runExecutor(t)
			e.run(t)
			if a = e.get(t, a.ID); a.State != privileged.StatePreflightFailed || !strings.Contains(deref(a.LastError), tc.want) {
				t.Fatalf("action = %+v (%v), want %q", a, deref(a.LastError), tc.want)
			}
			if n := len(e.openItems(t)); n != 0 {
				t.Fatalf("items = %d", n)
			}
		})
	}
	for name, tc := range map[string]struct {
		typ  string
		args map[string]any
	}{
		"restart a CronJob":     {privileged.TypeRolloutRestart, map[string]any{"namespace": appNS, "kind": "CronJob", "name": "x"}},
		"scale a DaemonSet":     {privileged.TypeScale, map[string]any{"namespace": appNS, "kind": "DaemonSet", "name": "x", "replicas": 1}},
		"too many replicas":     {privileged.TypeScale, map[string]any{"namespace": appNS, "kind": "Deployment", "name": "x", "replicas": 101}},
		"no replicas":           {privileged.TypeScale, map[string]any{"namespace": appNS, "kind": "Deployment", "name": "x"}},
		"restart with replicas": {privileged.TypeRolloutRestart, map[string]any{"namespace": appNS, "kind": "Deployment", "name": "x", "replicas": 1}},
		"bad kind":              {privileged.TypeDelete, map[string]any{"namespace": appNS, "kind": "pods/log", "name": "x"}},
		"bad name":              {privileged.TypeDelete, map[string]any{"namespace": appNS, "kind": "Pod", "name": "a/b"}},
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

type lostDelete struct {
	privileged.Kube
	deletes int
}

func (l *lostDelete) Delete(ctx context.Context, r privileged.Resource, ns, name, uid string) error {
	l.deletes++
	if err := l.Kube.Delete(ctx, r, ns, name, uid); err != nil {
		return err
	}
	return errors.New("connection reset by peer")
}
