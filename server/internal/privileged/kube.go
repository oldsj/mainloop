package privileged

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// The executor's narrow ServiceAccounts, in its own namespace. It holds none of their rights: each
// run of a k8s.* type mints a 10-minute token for the one it needs.
const (
	// AccountWorkloads may get and patch Deployments, StatefulSets and DaemonSets, and list
	// HorizontalPodAutoscalers: k8s.rollout_restart and k8s.scale.
	AccountWorkloads = "mainloop-executor-workloads"
	// AccountDelete may get and delete any object: k8s.delete. The executor reads through it with
	// metadata-only requests, so no object's data (a Secret's included) reaches the executor.
	AccountDelete = "mainloop-executor-delete"
	// tokenSeconds is the minted tokens' lifetime, the shortest the API server issues.
	tokenSeconds = 600
	// AnnotationRestartedAt is the pod template annotation a restart sets, as kubectl does.
	AnnotationRestartedAt = "kubectl.kubernetes.io/restartedAt"
)

// Workload is what preflight and the observe step read about a Deployment, StatefulSet or
// DaemonSet.
type Workload struct {
	Exists          bool   `json:"exists"`
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resource_version,omitempty"`
	// Generation counts spec changes: a status update leaves it alone.
	Generation int64 `json:"generation,omitempty"`
	// Replicas is the spec's; a DaemonSet has none.
	Replicas    *int32   `json:"replicas,omitempty"`
	Ready       int32    `json:"ready"`
	Updated     int32    `json:"updated"`
	Desired     int32    `json:"desired"`
	Paused      bool     `json:"paused,omitempty"`
	Images      []string `json:"images,omitempty"`
	RestartedAt string   `json:"restarted_at,omitempty"`
	ArgoTracked bool     `json:"argo_tracked,omitempty"`
	ManagedBy   string   `json:"managed_by,omitempty"`
	// Autoscaler names a HorizontalPodAutoscaler that targets it.
	Autoscaler string `json:"autoscaler,omitempty"`
	// Action is the privileged action that last wrote it, if any.
	Action string `json:"action,omitempty"`
}

// ObjectRef names any object: Kind is a kind or resource, optionally with its API group after a dot
// (Pod, deployments.apps, Certificate.cert-manager.io). Namespace is empty for a cluster-scoped one.
type ObjectRef struct {
	Kind      string
	Namespace string
	Name      string
}

// Resource is a kind resolved through the API server's discovery.
type Resource struct {
	Group      string `json:"group"`
	Version    string `json:"version"`
	Resource   string `json:"resource"`
	Kind       string `json:"kind"`
	Namespaced bool   `json:"namespaced"`
}

func (r Resource) gvr() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: r.Group, Version: r.Version, Resource: r.Resource}
}

// Qualified is the kind with its group, as a card shows it.
func (r Resource) Qualified() string {
	if r.Group == "" {
		return r.Kind
	}
	return r.Kind + "." + r.Group
}

// ObjectMeta is what preflight and the observe step read about an object to delete: metadata only.
type ObjectMeta struct {
	Exists          bool     `json:"exists"`
	UID             string   `json:"uid,omitempty"`
	ResourceVersion string   `json:"resource_version,omitempty"`
	CreatedAt       string   `json:"created_at,omitempty"`
	Terminating     bool     `json:"terminating,omitempty"`
	Owners          []string `json:"owners,omitempty"`
	Controller      string   `json:"controller,omitempty"`
	Finalizers      []string `json:"finalizers,omitempty"`
	ArgoTracked     bool     `json:"argo_tracked,omitempty"`
	ManagedBy       string   `json:"managed_by,omitempty"`
}

// Kube reads and writes the targets of the k8s.* types.
type Kube interface {
	// Workload reads a Deployment, StatefulSet or DaemonSet; a missing one has Exists false.
	Workload(ctx context.Context, kind, namespace, name string) (Workload, error)
	// Restart sets the pod template's restartedAt to at, and the action annotation, only at the
	// workload's ResourceVersion: a workload changed since is ErrTargetChanged.
	Restart(ctx context.Context, w Workload, at, actionID string) (string, error)
	// Scale sets spec.replicas, and the action annotation, only at the workload's ResourceVersion.
	Scale(ctx context.Context, w Workload, replicas int32, actionID string) (string, error)
	// Resolve maps a kind to its resource. An unknown or ambiguous kind is ErrBadTarget.
	Resolve(ctx context.Context, kind string) (Resource, error)
	// Object reads an object's metadata; a missing one has Exists false.
	Object(ctx context.Context, r Resource, namespace, name string) (ObjectMeta, error)
	// Delete deletes the object only if its UID is still uid: a replaced one is ErrTargetChanged,
	// a missing one ErrGone.
	Delete(ctx context.Context, r Resource, namespace, name, uid string) error

	// ResolveObject maps a manifest's apiVersion and kind to its resource.
	ResolveObject(ctx context.Context, apiVersion, kind string) (Resource, error)
	// Get reads a whole object (never a Secret: the types that call it refuse them first); a
	// missing one is nil.
	Get(ctx context.Context, r Resource, namespace, name string) (map[string]any, error)
	// Patch patches an object, or with dryRun returns what the API server would store. A
	// resourceVersion in the patch is its precondition: a changed object is ErrTargetChanged.
	Patch(ctx context.Context, r Resource, namespace, name string, pt types.PatchType, body []byte, dryRun bool) (map[string]any, error)
	// Apply server-side applies obj, or with dryRun returns what the API server would store. A
	// resourceVersion in obj is its precondition; fields another manager owns are
	// ErrFieldConflict unless force.
	Apply(ctx context.Context, r Resource, obj map[string]any, force, dryRun bool) (map[string]any, error)

	// App reads an Argo CD Application; a missing one has Exists false.
	App(ctx context.Context, namespace, name string) (ArgoApp, error)
	// Sync starts a sync of app at its revisions, only at its ResourceVersion: a changed
	// Application is ErrTargetChanged.
	Sync(ctx context.Context, app ArgoApp, prune bool, actionID string) error
}

// ErrGone is a target that no longer exists.
var ErrGone = errors.New("privileged: the target no longer exists")

// KubeClients mints the executor's per-type credentials: a token for one of its narrow
// ServiceAccounts, valid for 10 minutes, and a client built on it.
type KubeClients struct {
	// Client is the executor's own: it may only create tokens for the narrow ServiceAccounts.
	Client kubernetes.Interface
	// Base is the in-cluster config the minted clients copy (server and CA, not credentials).
	Base *rest.Config
	// Namespace is the executor's, where the narrow ServiceAccounts live.
	Namespace string
}

// Config is a client config that authenticates as account with a fresh token.
func (c KubeClients) Config(ctx context.Context, account string) (*rest.Config, error) {
	ttl := int64(tokenSeconds)
	tr, err := c.Client.CoreV1().ServiceAccounts(c.Namespace).CreateToken(ctx, account,
		&authv1.TokenRequest{Spec: authv1.TokenRequestSpec{ExpirationSeconds: &ttl}}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("mint a token for %s: %w", account, kubeError(err))
	}
	cfg := rest.AnonymousClientConfig(c.Base)
	cfg.BearerToken = tr.Status.Token
	return cfg, nil
}

// KubeActions is Kube over clients built on minted tokens. Tests set the client functions to
// fakes.
type KubeActions struct {
	// Workloads returns a client authenticated as AccountWorkloads.
	Workloads func(ctx context.Context) (kubernetes.Interface, error)
	// Deleter returns a metadata client authenticated as AccountDelete, and a mapper over the API
	// server's discovery.
	Deleter func(ctx context.Context) (metadata.Interface, meta.RESTMapper, error)
	// Writer returns a dynamic client authenticated as AccountWriter, and a mapper over the API
	// server's discovery.
	Writer writerFunc
	// Argo returns a dynamic client authenticated as AccountArgo.
	Argo func(ctx context.Context) (dynamic.Interface, error)
}

// NewKubeActions mints tokens through c. Each client, with its discovery cache, is reused for
// tokenReuse, well inside the token's 10 minutes, so an attempt over many objects mints one token
// per account rather than one per call.
func NewKubeActions(c KubeClients) KubeActions {
	workloads := &minted[kubernetes.Interface]{make: func(ctx context.Context) (kubernetes.Interface, error) {
		cfg, err := c.Config(ctx, AccountWorkloads)
		if err != nil {
			return nil, err
		}
		return kubernetes.NewForConfig(cfg)
	}}
	type deleter struct {
		mc     metadata.Interface
		mapper meta.RESTMapper
	}
	deleters := &minted[deleter]{make: func(ctx context.Context) (deleter, error) {
		cfg, err := c.Config(ctx, AccountDelete)
		if err != nil {
			return deleter{}, err
		}
		mc, err := metadata.NewForConfig(cfg)
		if err != nil {
			return deleter{}, err
		}
		dc, err := discovery.NewDiscoveryClientForConfig(cfg)
		if err != nil {
			return deleter{}, err
		}
		return deleter{mc, restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))}, nil
	}}
	type writer struct {
		dc     dynamic.Interface
		mapper meta.RESTMapper
	}
	writers := &minted[writer]{make: func(ctx context.Context) (writer, error) {
		dc, mapper, err := newWriter(c)(ctx)
		return writer{dc, mapper}, err
	}}
	return KubeActions{
		Workloads: workloads.get,
		Deleter: func(ctx context.Context) (metadata.Interface, meta.RESTMapper, error) {
			d, err := deleters.get(ctx)
			return d.mc, d.mapper, err
		},
		Writer: func(ctx context.Context) (dynamic.Interface, meta.RESTMapper, error) {
			w, err := writers.get(ctx)
			return w.dc, w.mapper, err
		},
		Argo: (&minted[dynamic.Interface]{make: newArgo(c)}).get,
	}
}

// tokenReuse is how long a client on a minted token is reused.
const tokenReuse = 5 * time.Minute

// minted holds one client built on a minted token until tokenReuse passes.
type minted[T any] struct {
	make  func(ctx context.Context) (T, error)
	mu    sync.Mutex
	value T
	until time.Time
}

func (m *minted[T]) get(ctx context.Context) (T, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.until) {
		return m.value, nil
	}
	v, err := m.make(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	m.value, m.until = v, time.Now().Add(tokenReuse)
	return v, nil
}

// WorkloadKinds are the kinds k8s.rollout_restart accepts; k8s.scale takes the first two.
var WorkloadKinds = []string{"Deployment", "StatefulSet", "DaemonSet"}

// Workload reads the workload and the autoscaler that targets it, if any.
func (k KubeActions) Workload(ctx context.Context, kind, namespace, name string) (Workload, error) {
	c, err := k.Workloads(ctx)
	if err != nil {
		return Workload{}, err
	}
	w := Workload{Kind: kind, Namespace: namespace, Name: name}
	var (
		om       metav1.ObjectMeta
		template corev1.PodTemplateSpec
	)
	switch kind {
	case "Deployment":
		d, err := c.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return notFoundOr(w, err)
		}
		om, template = d.ObjectMeta, d.Spec.Template
		w.Replicas, w.Paused = replicas(d.Spec.Replicas), d.Spec.Paused
		w.Ready, w.Updated, w.Desired = d.Status.ReadyReplicas, d.Status.UpdatedReplicas, *w.Replicas
	case "StatefulSet":
		s, err := c.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return notFoundOr(w, err)
		}
		om, template = s.ObjectMeta, s.Spec.Template
		w.Replicas = replicas(s.Spec.Replicas)
		w.Ready, w.Updated, w.Desired = s.Status.ReadyReplicas, s.Status.UpdatedReplicas, *w.Replicas
	case "DaemonSet":
		d, err := c.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return notFoundOr(w, err)
		}
		om, template = d.ObjectMeta, d.Spec.Template
		w.Ready, w.Updated, w.Desired = d.Status.NumberReady, d.Status.UpdatedNumberScheduled, d.Status.DesiredNumberScheduled
	default:
		return Workload{}, fmt.Errorf("%w: kind %q is not a workload", ErrBadTarget, kind)
	}
	w.Exists, w.UID, w.ResourceVersion, w.Generation = true, string(om.UID), om.ResourceVersion, om.Generation
	w.ArgoTracked, w.ManagedBy, w.Action = argoTracked(om), om.Labels["app.kubernetes.io/managed-by"], om.Annotations[AnnotationAction]
	w.RestartedAt = template.Annotations[AnnotationRestartedAt]
	for _, ctr := range template.Spec.Containers {
		w.Images = append(w.Images, ctr.Image)
	}
	if kind != "DaemonSet" {
		w.Autoscaler = autoscaler(ctx, c, kind, namespace, name)
	}
	return w, nil
}

func replicas(n *int32) *int32 {
	if n == nil {
		one := int32(1)
		return &one
	}
	v := *n
	return &v
}

// autoscaler names the HorizontalPodAutoscaler that targets the workload. A list that fails is
// treated as none: it only adds a risk tag.
func autoscaler(ctx context.Context, c kubernetes.Interface, kind, namespace, name string) string {
	list, err := c.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}
	for _, h := range list.Items {
		if h.Spec.ScaleTargetRef.Kind == kind && h.Spec.ScaleTargetRef.Name == name {
			return h.Name
		}
	}
	return ""
}

func notFoundOr(w Workload, err error) (Workload, error) {
	if apierrors.IsNotFound(err) {
		return w, nil
	}
	return Workload{}, kubeError(err)
}

func argoTracked(om metav1.ObjectMeta) bool {
	return om.Annotations["argocd.argoproj.io/tracking-id"] != "" || om.Labels["argocd.argoproj.io/instance"] != ""
}

// Restart patches the pod template's restartedAt annotation.
func (k KubeActions) Restart(ctx context.Context, w Workload, at, actionID string) (string, error) {
	return k.patchWorkload(ctx, w, actionID, map[string]any{
		"template": map[string]any{"metadata": map[string]any{"annotations": map[string]string{AnnotationRestartedAt: at}}},
	})
}

// Scale patches spec.replicas.
func (k KubeActions) Scale(ctx context.Context, w Workload, n int32, actionID string) (string, error) {
	return k.patchWorkload(ctx, w, actionID, map[string]any{"replicas": n})
}

// patchWorkload merges spec into the workload, with the action annotation, at w's resourceVersion.
func (k KubeActions) patchWorkload(ctx context.Context, w Workload, actionID string, spec map[string]any) (string, error) {
	c, err := k.Workloads(ctx)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": w.ResourceVersion, "annotations": map[string]string{AnnotationAction: actionID}},
		"spec":     spec,
	})
	if err != nil {
		return "", err
	}
	opts := metav1.PatchOptions{FieldManager: FieldManager}
	var rv string
	switch w.Kind {
	case "Deployment":
		var d *appsv1.Deployment
		if d, err = c.AppsV1().Deployments(w.Namespace).Patch(ctx, w.Name, types.MergePatchType, body, opts); err == nil {
			rv = d.ResourceVersion
		}
	case "StatefulSet":
		var s *appsv1.StatefulSet
		if s, err = c.AppsV1().StatefulSets(w.Namespace).Patch(ctx, w.Name, types.MergePatchType, body, opts); err == nil {
			rv = s.ResourceVersion
		}
	case "DaemonSet":
		var d *appsv1.DaemonSet
		if d, err = c.AppsV1().DaemonSets(w.Namespace).Patch(ctx, w.Name, types.MergePatchType, body, opts); err == nil {
			rv = d.ResourceVersion
		}
	default:
		return "", fmt.Errorf("%w: kind %q is not a workload", ErrBadTarget, w.Kind)
	}
	switch {
	case apierrors.IsConflict(err) || apierrors.IsNotFound(err):
		return "", fmt.Errorf("%w: it changed or was deleted meanwhile", ErrTargetChanged)
	case err != nil:
		return "", kubeError(err)
	}
	return rv, nil
}

// Resolve finds the kind's resource: "Kind" or "resource", optionally followed by ".group".
func (k KubeActions) Resolve(ctx context.Context, kind string) (Resource, error) {
	_, mapper, err := k.Deleter(ctx)
	if err != nil {
		return Resource{}, err
	}
	name, group, _ := strings.Cut(kind, ".")
	gvr, err := mapper.ResourceFor(schema.GroupVersionResource{Group: group, Resource: strings.ToLower(name)})
	if err != nil {
		if meta.IsNoMatchError(err) || meta.IsAmbiguousError(err) {
			return Resource{}, fmt.Errorf("%w: kind %q: %s", ErrBadTarget, kind, discoveryProblem(err))
		}
		return Resource{}, err
	}
	gvk, err := mapper.KindFor(gvr)
	if err != nil {
		return Resource{}, err
	}
	m, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return Resource{}, err
	}
	return Resource{
		Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource, Kind: gvk.Kind,
		Namespaced: m.Scope.Name() == meta.RESTScopeNameNamespace,
	}, nil
}

func discoveryProblem(err error) string {
	if meta.IsAmbiguousError(err) {
		return "it names more than one kind; add the API group (Kind.group)"
	}
	return "the cluster has no such kind"
}

// Object reads the object's metadata only.
func (k KubeActions) Object(ctx context.Context, r Resource, namespace, name string) (ObjectMeta, error) {
	mc, _, err := k.Deleter(ctx)
	if err != nil {
		return ObjectMeta{}, err
	}
	o, err := resourceClient(mc, r, namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ObjectMeta{}, nil
	}
	if err != nil {
		return ObjectMeta{}, kubeError(err)
	}
	m := ObjectMeta{
		Exists: true, UID: string(o.UID), ResourceVersion: o.ResourceVersion, Terminating: o.DeletionTimestamp != nil,
		Finalizers: slices.Clone(o.Finalizers), ArgoTracked: argoTracked(o.ObjectMeta), ManagedBy: o.Labels["app.kubernetes.io/managed-by"],
	}
	if !o.CreationTimestamp.IsZero() {
		m.CreatedAt = o.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z")
	}
	for _, ref := range o.OwnerReferences {
		m.Owners = append(m.Owners, ref.Kind+"/"+ref.Name)
		if ref.Controller != nil && *ref.Controller {
			m.Controller = ref.Kind + "/" + ref.Name
		}
	}
	sort.Strings(m.Finalizers)
	return m, nil
}

// Delete deletes the object with its UID as the precondition, in the background as kubectl does.
func (k KubeActions) Delete(ctx context.Context, r Resource, namespace, name, uid string) error {
	mc, _, err := k.Deleter(ctx)
	if err != nil {
		return err
	}
	u := types.UID(uid)
	bg := metav1.DeletePropagationBackground
	err = resourceClient(mc, r, namespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &u}, PropagationPolicy: &bg,
	})
	switch {
	case apierrors.IsNotFound(err):
		return ErrGone
	case apierrors.IsConflict(err):
		return fmt.Errorf("%w: it was replaced by a new object", ErrTargetChanged)
	case err != nil:
		return kubeError(err)
	}
	return nil
}

func resourceClient(mc metadata.Interface, r Resource, namespace string) metadata.ResourceInterface {
	if r.Namespaced {
		return mc.Resource(r.gvr()).Namespace(namespace)
	}
	return mc.Resource(r.gvr())
}
