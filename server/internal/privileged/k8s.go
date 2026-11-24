package privileged

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// The typed Kubernetes types. serve-executor runs them, each with a token minted for one narrow
// ServiceAccount (kube.go).
const (
	TypeRolloutRestart = "k8s.rollout_restart"
	TypeScale          = "k8s.scale"
	TypeDelete         = "k8s.delete"
)

// Bounds of the Kubernetes types.
const (
	MaxReplicas = 100
	// DeleteExpiry is how long a delete waits for the owner.
	DeleteExpiry = time.Hour
	maxKindLen   = 253
)

const kubeHow = "Approve runs exactly this once, through Mainloop's executor with a 10-minute credential for this kind of change only."

// WorkloadArgs are k8s.rollout_restart's arguments, and k8s.scale's with Replicas.
type WorkloadArgs struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Replicas  *int32 `json:"replicas,omitempty"`
}

// DeleteArgs are k8s.delete's arguments. Namespace is empty for a cluster-scoped object.
type DeleteArgs struct {
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
}

// WorkloadFacts are k8s.rollout_restart's and k8s.scale's frozen facts.
type WorkloadFacts struct {
	Cluster   string    `json:"cluster,omitempty"`
	Workload  Workload  `json:"workload"`
	CheckedAt time.Time `json:"checked_at"`
}

// DeleteFacts are k8s.delete's frozen facts.
type DeleteFacts struct {
	Cluster   string     `json:"cluster,omitempty"`
	Resource  Resource   `json:"resource"`
	Namespace string     `json:"namespace,omitempty"`
	Name      string     `json:"name"`
	Object    ObjectMeta `json:"object"`
	CheckedAt time.Time  `json:"checked_at"`
}

// WorkloadResult is k8s.rollout_restart's and k8s.scale's result.
type WorkloadResult struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	RestartedAt     string `json:"restarted_at,omitempty"`
	Replicas        *int32 `json:"replicas,omitempty"`
	ResourceVersion string `json:"resource_version,omitempty"`
	// Observed is set when an earlier attempt had already written it.
	Observed bool `json:"observed,omitempty"`
}

// DeleteResult is k8s.delete's result.
type DeleteResult struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	// FinalizersPending says the object waits for its finalizers before it is gone.
	FinalizersPending bool `json:"finalizers_pending,omitempty"`
	Observed          bool `json:"observed,omitempty"`
}

// workloadKind is the canonical kind for a kind or resource name, any case.
func workloadKind(s string, allowed []string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, k := range allowed {
		l := strings.ToLower(k)
		if s == l || s == l+"s" || s == l+".apps" || s == l+"s.apps" {
			return k, true
		}
	}
	return "", false
}

func normalizeWorkload(raw json.RawMessage, allowed []string, scale bool) (WorkloadArgs, error) {
	var a WorkloadArgs
	if err := decodeStrict(raw, &a); err != nil {
		return a, err
	}
	kind, ok := workloadKind(a.Kind, allowed)
	if !ok {
		return a, fmt.Errorf("%w: kind must be one of %s", ErrInvalid, strings.Join(allowed, ", "))
	}
	a.Kind = kind
	switch {
	case !dnsLabelRE.MatchString(a.Namespace):
		return a, fmt.Errorf("%w: namespace must be a Kubernetes namespace name", ErrInvalid)
	case !dnsSubdomainRE.MatchString(a.Name):
		return a, fmt.Errorf("%w: name must be a Kubernetes object name", ErrInvalid)
	case scale && (a.Replicas == nil || *a.Replicas < 0 || *a.Replicas > MaxReplicas):
		return a, fmt.Errorf("%w: replicas must be 0 to %d", ErrInvalid, MaxReplicas)
	case !scale && a.Replicas != nil:
		return a, fmt.Errorf("%w: args: unknown field \"replicas\"", ErrInvalid)
	}
	return a, nil
}

func normalizeRestart(raw json.RawMessage) (any, error) {
	return normalizeWorkload(raw, WorkloadKinds, false)
}

func normalizeScale(raw json.RawMessage) (any, error) {
	return normalizeWorkload(raw, WorkloadKinds[:2], true)
}

func normalizeDelete(raw json.RawMessage) (any, error) {
	var a DeleteArgs
	if err := decodeStrict(raw, &a); err != nil {
		return nil, err
	}
	kind, err := normalizeRef(a.Namespace, a.Kind, a.Name)
	if err != nil {
		return nil, err
	}
	a.Kind = kind
	return a, nil
}

// normalizeRef validates an object reference by kind and returns the trimmed kind.
func normalizeRef(namespace, kind, name string) (string, error) {
	kind = strings.TrimSpace(kind)
	k, group, _ := strings.Cut(kind, ".")
	switch {
	case k == "" || len(kind) > maxKindLen || strings.ContainsAny(k, "/ ") ||
		(group != "" && !dnsSubdomainRE.MatchString(group)):
		return "", fmt.Errorf("%w: kind must be a kind or resource, optionally with its API group (Pod, Job.batch)", ErrInvalid)
	case namespace != "" && !dnsLabelRE.MatchString(namespace):
		return "", fmt.Errorf("%w: namespace must be a Kubernetes namespace name, or empty for a cluster-scoped object", ErrInvalid)
	case !validObjectName(name):
		return "", fmt.Errorf("%w: name must be a Kubernetes object name", ErrInvalid)
	}
	return kind, nil
}

// validObjectName accepts the names the API server allows for most kinds: a DNS subdomain, or the
// colon-separated names RBAC objects use.
func validObjectName(s string) bool {
	if s == "" || len(s) > 253 || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, "/%") && !strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r > '~' })
}

// selfTarget says whether a target is the executor's own: its namespace, or the cluster-scoped
// objects the chart names after it (its ClusterRoles and their bindings). Those change only
// through GitOps the owner merges.
func (s *Service) selfTarget(namespace, kind, name string, namespaced bool) bool {
	own := s.ExecutorNamespace
	if own == "" {
		return false
	}
	switch {
	case namespaced:
		return namespace == own
	case kind == "Namespace":
		return name == own
	}
	return strings.HasPrefix(name, own+"-")
}

func (s *Service) kubeReady() error {
	if s.Kube == nil {
		return refuse("this executor has no Kubernetes access")
	}
	return nil
}

func preflightRestart(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
	return preflightWorkload(ctx, s, raw, now, false)
}

func preflightScale(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
	return preflightWorkload(ctx, s, raw, now, true)
}

func preflightWorkload(ctx context.Context, s *Service, raw json.RawMessage, now time.Time, scale bool) (any, error) {
	var a WorkloadArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if err := s.kubeReady(); err != nil {
		return nil, err
	}
	if s.selfTarget(a.Namespace, a.Kind, a.Name, true) {
		return nil, refuse("namespace %s is the executor's own; it changes only through GitOps", a.Namespace)
	}
	w, err := s.Kube.Workload(ctx, a.Kind, a.Namespace, a.Name)
	if err != nil {
		return nil, badTarget(err)
	}
	switch {
	case !w.Exists:
		return nil, refuse("%s %s/%s was not found", a.Kind, a.Namespace, a.Name)
	case scale && *w.Replicas == *a.Replicas:
		return nil, refuse("%s %s/%s already has %d replicas", a.Kind, a.Namespace, a.Name, *a.Replicas)
	}
	return WorkloadFacts{Cluster: s.Cluster, Workload: w, CheckedAt: now}, nil
}

func preflightDelete(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
	var a DeleteArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if err := s.kubeReady(); err != nil {
		return nil, err
	}
	r, err := s.Kube.Resolve(ctx, a.Kind)
	if err != nil {
		return nil, badTarget(err)
	}
	switch {
	case r.Namespaced && a.Namespace == "":
		return nil, refuse("%s is namespaced; give its namespace", r.Qualified())
	case !r.Namespaced && a.Namespace != "":
		return nil, refuse("%s is cluster-scoped; leave namespace empty", r.Qualified())
	case s.selfTarget(a.Namespace, r.Kind, a.Name, r.Namespaced):
		return nil, refuse("%s is the executor's own; it changes only through GitOps", objectName(r, a.Namespace, a.Name))
	}
	o, err := s.Kube.Object(ctx, r, a.Namespace, a.Name)
	if err != nil {
		return nil, badTarget(err)
	}
	switch {
	case !o.Exists:
		return nil, refuse("%s was not found", objectName(r, a.Namespace, a.Name))
	case o.Terminating:
		return nil, refuse("%s is already being deleted (finalizers: %s)", objectName(r, a.Namespace, a.Name), orNone(o.Finalizers))
	}
	return DeleteFacts{Cluster: s.Cluster, Resource: r, Namespace: a.Namespace, Name: a.Name, Object: o, CheckedAt: now}, nil
}

// badTarget turns a target the action cannot apply to, or a credential that may not read it, into
// a refusal; other errors are retried.
func badTarget(err error) error {
	switch {
	case errors.Is(err, ErrBadTarget):
		return refuse("%s", strings.TrimPrefix(err.Error(), ErrBadTarget.Error()+": "))
	case errors.Is(err, ErrForbidden):
		return refuse("the executor's credential for this type may not read it")
	}
	return err
}

func objectName(r Resource, namespace, name string) string {
	if namespace == "" {
		return r.Qualified() + " " + name
	}
	return r.Qualified() + " " + namespace + "/" + name
}

func onCluster(cluster string) string {
	if cluster == "" {
		return ""
	}
	return " on " + cluster
}

// namespaceTags are the risk tags of a change in a namespace.
func namespaceTags(ns string) []string {
	switch {
	case strings.HasPrefix(ns, "mainloop"):
		return []string{"touches Mainloop's control plane"}
	case strings.HasPrefix(ns, "kube-"):
		return []string{"cluster system namespace"}
	case ns == "argocd":
		return []string{"Argo CD's own namespace"}
	}
	return nil
}

func workloadLine(w Workload) string {
	var b strings.Builder
	if w.Replicas != nil {
		fmt.Fprintf(&b, "Now: %d replicas, %d ready, %d updated.", *w.Replicas, w.Ready, w.Updated)
	} else {
		fmt.Fprintf(&b, "Now: %d desired, %d ready, %d updated.", w.Desired, w.Ready, w.Updated)
	}
	if len(w.Images) > 0 {
		fmt.Fprintf(&b, " Images: %s.", strings.Join(w.Images, ", "))
	}
	return b.String()
}

func renderWorkload(r dbq.PrivilegedAction, ctx map[string]any) (string, string, map[string]any, error) {
	var f WorkloadFacts
	if err := json.Unmarshal(r.FrozenFacts, &f); err != nil {
		return "", "", nil, err
	}
	w := f.Workload
	target := fmt.Sprintf("%s %s/%s", w.Kind, w.Namespace, w.Name)
	tags := namespaceTags(w.Namespace)
	var title string
	var b strings.Builder
	if r.Type == TypeScale {
		var a WorkloadArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return "", "", nil, err
		}
		n := *a.Replicas
		title = fmt.Sprintf("Scale %s to %d%s", target, n, onCluster(f.Cluster))
		fmt.Fprintf(&b, "Sets spec.replicas of %s from %d to %d.\n", target, *w.Replicas, n)
		if n == 0 {
			tags = append(tags, "destructive: scales to zero")
		}
		if w.Autoscaler != "" {
			tags = append(tags, "HorizontalPodAutoscaler "+w.Autoscaler+" manages its replicas and may override this")
		}
		if w.ArgoTracked {
			tags = append(tags, "Argo-managed: Argo CD may revert it, or it drifts from Git")
		}
	} else {
		title = fmt.Sprintf("Restart %s%s", target, onCluster(f.Cluster))
		fmt.Fprintf(&b, "Rolling restart, as `kubectl rollout restart` does: sets the pod template's %s, and the controller "+
			"replaces the pods.\n", AnnotationRestartedAt)
		if w.RestartedAt != "" {
			fmt.Fprintf(&b, "Last restarted %s.\n", w.RestartedAt)
		}
		if w.Paused {
			tags = append(tags, "paused: the restart takes effect when it is resumed")
		}
		if w.ArgoTracked {
			tags = append(tags, "Argo-managed: the restart annotation shows as drift")
		}
	}
	b.WriteString(workloadLine(w) + "\n")
	fmt.Fprintf(&b, "If its spec changes (generation %d) or it is replaced before this runs, nothing is written.", w.Generation)
	if len(tags) > 0 {
		fmt.Fprintf(&b, "\nRisk: %s.", strings.Join(tags, ", "))
	}
	ctx["namespace"] = w.Namespace
	return title, b.String(), ctx, nil
}

func renderDelete(r dbq.PrivilegedAction, ctx map[string]any) (string, string, map[string]any, error) {
	var f DeleteFacts
	if err := json.Unmarshal(r.FrozenFacts, &f); err != nil {
		return "", "", nil, err
	}
	o := f.Object
	target := objectName(f.Resource, f.Namespace, f.Name)
	title := "Delete " + target + onCluster(f.Cluster)
	var b strings.Builder
	fmt.Fprintf(&b, "Deletes %s (uid %s", target, o.UID)
	if o.CreatedAt != "" {
		fmt.Fprintf(&b, ", created %s", o.CreatedAt)
	}
	b.WriteString("), with background propagation as `kubectl delete` does. Only this object: if it is replaced by a " +
		"new one with the same name before this runs, nothing is deleted.\n")
	if len(o.Owners) > 0 {
		fmt.Fprintf(&b, "Owners: %s.\n", strings.Join(o.Owners, ", "))
	}
	if len(o.Finalizers) > 0 {
		fmt.Fprintf(&b, "Finalizers: %s; it is gone only once they finish.\n", strings.Join(o.Finalizers, ", "))
	}
	if o.ManagedBy != "" {
		fmt.Fprintf(&b, "Labelled managed-by %s.\n", o.ManagedBy)
	}
	tags := []string{"destructive", "irreversible"}
	switch {
	case f.Resource.Kind == "Namespace" && f.Resource.Group == "":
		tags = append(tags, "deletes everything in the namespace")
		tags = append(tags, namespaceTags(f.Name)...)
	case f.Resource.Kind == "CustomResourceDefinition" && f.Resource.Group == "apiextensions.k8s.io":
		tags = append(tags, "deletes every "+f.Name+" object in the cluster, in every namespace")
	case f.Resource.Kind == "APIService" && f.Resource.Group == "apiregistration.k8s.io":
		tags = append(tags, "the API it serves ("+f.Name+") goes away for every client and controller")
	default:
		tags = append(tags, namespaceTags(f.Namespace)...)
	}
	if o.Controller != "" {
		tags = append(tags, "owned by "+o.Controller+", which may recreate it")
	}
	if o.ArgoTracked {
		tags = append(tags, "Argo-managed: Argo CD may recreate it")
	}
	fmt.Fprintf(&b, "Risk: %s.", strings.Join(tags, ", "))
	if f.Namespace != "" {
		ctx["namespace"] = f.Namespace
	}
	return title, b.String(), ctx, nil
}

// restartAt is the restartedAt value of an action: its approval time, the same on every attempt.
func restartAt(row dbq.PrivilegedAction) string {
	t := row.CreatedAt
	if row.DecidedAt != nil {
		t = *row.DecidedAt
	}
	return t.UTC().Format(time.RFC3339)
}

// workload runs k8s.rollout_restart and k8s.scale. The owner approved a change to the object as it
// was: the same UID and spec generation. Status updates do not count, so the write carries the
// resourceVersion just observed, and a concurrent write retries through a new observation.
func (r *Runner) workload(ctx context.Context, row dbq.PrivilegedAction) outcome {
	var f WorkloadFacts
	var a WorkloadArgs
	if json.Unmarshal(row.FrozenFacts, &f) != nil || json.Unmarshal(row.Args, &a) != nil {
		return outcome{state: StateFailed, err: "the frozen facts cannot be read"}
	}
	if r.Kube == nil {
		return outcome{state: StateFailed, err: "this executor has no Kubernetes access"}
	}
	was := f.Workload
	res := WorkloadResult{Kind: was.Kind, Namespace: was.Namespace, Name: was.Name}
	now, err := r.Kube.Workload(ctx, was.Kind, was.Namespace, was.Name)
	if err != nil {
		return kubeFailure(err)
	}
	switch {
	case now.Exists && now.Action == row.ID:
		res.RestartedAt, res.ResourceVersion, res.Observed = now.RestartedAt, now.ResourceVersion, true
		if row.Type == TypeScale {
			res.RestartedAt, res.Replicas = "", now.Replicas
		}
		return outcome{state: StateSucceeded, result: res}
	case !now.Exists:
		return outcome{state: StateStale, err: "it was deleted since the owner saw it"}
	case now.UID != was.UID:
		return outcome{state: StateStale, err: "it was replaced by a new object since the owner saw it"}
	case now.Generation != was.Generation:
		return outcome{state: StateStale, err: fmt.Sprintf("its spec changed since the owner saw it (generation %d, now %d)", was.Generation, now.Generation)}
	}
	if row.Type == TypeScale {
		res.Replicas = a.Replicas
		res.ResourceVersion, err = r.Kube.Scale(ctx, now, *a.Replicas, row.ID)
	} else {
		res.RestartedAt = restartAt(row)
		res.ResourceVersion, err = r.Kube.Restart(ctx, now, res.RestartedAt, row.ID)
	}
	if err != nil {
		return kubeFailure(err) // a concurrent write retries; the next attempt observes
	}
	return outcome{state: StateSucceeded, result: res}
}

// kubeFailure ends an attempt that a Kubernetes call failed: forbidden or rejected fails the
// action, anything else (a timeout, a lost answer, a conflict) retries, observing first.
func kubeFailure(err error) outcome {
	if errors.Is(err, ErrForbidden) || errors.Is(err, ErrRejected) {
		return outcome{state: StateFailed, err: safeError(err)}
	}
	return retry(err)
}

// deleteObject runs k8s.delete with the frozen UID as the precondition. On the first attempt an object
// already gone or replaced is stale: someone else acted. On a later one it is this action's own
// earlier delete, whose answer was lost.
func (r *Runner) deleteObject(ctx context.Context, row dbq.PrivilegedAction) outcome {
	var f DeleteFacts
	if err := json.Unmarshal(row.FrozenFacts, &f); err != nil {
		return outcome{state: StateFailed, err: "the frozen facts cannot be read"}
	}
	if r.Kube == nil {
		return outcome{state: StateFailed, err: "this executor has no Kubernetes access"}
	}
	res := DeleteResult{Kind: f.Resource.Qualified(), Namespace: f.Namespace, Name: f.Name, UID: f.Object.UID}
	first := row.Attempts == 1
	gone := func(why string) outcome {
		if first {
			return outcome{state: StateStale, err: why}
		}
		res.Observed = true
		return outcome{state: StateSucceeded, result: res}
	}
	now, err := r.Kube.Object(ctx, f.Resource, f.Namespace, f.Name)
	if err != nil {
		return kubeFailure(err)
	}
	switch {
	case !now.Exists:
		return gone("it was already deleted by someone else")
	case now.UID != f.Object.UID:
		return gone("it was replaced by a new object since the owner saw it")
	case now.Terminating && !first:
		res.FinalizersPending, res.Observed = true, true
		return outcome{state: StateSucceeded, result: res}
	}
	err = r.Kube.Delete(ctx, f.Resource, f.Namespace, f.Name, f.Object.UID)
	switch {
	case errors.Is(err, ErrGone):
		return gone("it was deleted by someone else meanwhile")
	case errors.Is(err, ErrTargetChanged):
		return outcome{state: StateStale, err: "it was replaced by a new object since the owner saw it; nothing was deleted"}
	case err != nil:
		return kubeFailure(err)
	}
	res.FinalizersPending = len(f.Object.Finalizers) > 0
	return outcome{state: StateSucceeded, result: res}
}
