package privileged

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// TypeArgoSync starts an Argo CD sync of one Application at the revision the owner saw.
const TypeArgoSync = "argocd.sync"

// AccountArgo may get and patch Argo CD Applications: argocd.sync.
const AccountArgo = "mainloop-executor-argo"

// DefaultArgoNamespace is where Applications live unless the request names another.
const DefaultArgoNamespace = "argocd"

var applicationsGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// ArgoArgs are argocd.sync's arguments.
type ArgoArgs struct {
	Namespace string `json:"namespace,omitempty"`
	App       string `json:"app"`
	Prune     bool   `json:"prune,omitempty"`
}

// ArgoResource is one resource of an Application that is out of sync or needs pruning.
type ArgoResource struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Prune     bool   `json:"prune,omitempty"`
}

func (r ArgoResource) String() string {
	s := r.Kind + " " + strings.TrimPrefix(r.Namespace+"/"+r.Name, "/")
	if r.Prune {
		return s + " (to prune)"
	}
	return s
}

// ArgoApp is what preflight and the observe step read about an Application.
type ArgoApp struct {
	Exists    bool   `json:"exists"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Destination is the namespace the Application deploys to by default, and DestinationServer
	// the cluster (its URL or name), as spec.destination gives them.
	Destination       string `json:"destination,omitempty"`
	DestinationServer string `json:"destination_server,omitempty"`
	UID               string `json:"uid,omitempty"`
	ResourceVersion   string `json:"resource_version,omitempty"`
	// Revisions are the synced-to revisions Argo CD resolved: one, or one per source.
	Revisions   []string       `json:"revisions,omitempty"`
	MultiSource bool           `json:"multi_source,omitempty"`
	Sync        string         `json:"sync,omitempty"`
	Health      string         `json:"health,omitempty"`
	Automated   bool           `json:"automated,omitempty"`
	OutOfSync   []ArgoResource `json:"out_of_sync,omitempty"`
	// Operation is the phase of a sync in progress, or empty.
	Operation string `json:"operation,omitempty"`
	// Action is the privileged action that last started a sync of it, if any, and InitiatedBy the
	// initiator of its last sync operation (mainloop/<action id> for one an action started).
	Action      string `json:"action,omitempty"`
	InitiatedBy string `json:"-"`
	// SyncOptions and Retry are spec.syncPolicy's: the argocd server copies them into every sync
	// operation it starts, and so does argocd.sync.
	SyncOptions []string       `json:"sync_options,omitempty"`
	Retry       map[string]any `json:"retry,omitempty"`
	// Sources is spec.source, or spec.sources, as JSON: bound between approval and the sync. The
	// agent does not get it (inline Helm values can hold anything); the card summarises it.
	Sources json.RawMessage `json:"sources,omitempty"`
	// Namespaces are the namespaces of the resources Argo CD manages for it (status.resources),
	// and the names of the Namespace objects among them.
	Namespaces []string `json:"namespaces,omitempty"`
	// clusterScoped are the names of the other cluster-scoped resources it manages.
	clusterScoped []string
}

// deploysInto says how the Application deploys into namespace own (the executor's), or "": its
// destination, a resource there, the Namespace itself, or a cluster-scoped object named after it
// (the executor's ClusterRoles and bindings are "<own>-…"). The Mainloop chart renders the
// executor's objects into own from a release in another namespace, so the destination alone
// misses it.
func (app ArgoApp) deploysInto(own string) string {
	switch {
	case own == "":
		return ""
	case app.Destination == own:
		return "deploys to the executor's own namespace"
	case slices.Contains(app.Namespaces, own):
		return "manages resources in the executor's own namespace"
	}
	for _, name := range app.clusterScoped {
		if strings.HasPrefix(name, own+"-") {
			return "manages " + name + ", one of the executor's own cluster-wide objects"
		}
	}
	return ""
}

// specChange names what of the Application's spec differs from what the owner saw, or "".
func (app ArgoApp) specChange(was ArgoApp) string {
	switch {
	case app.Destination != was.Destination || app.DestinationServer != was.DestinationServer:
		return "its destination"
	case !jsonEqual(app.Sources, was.Sources):
		return "its source"
	case !slices.Equal(app.SyncOptions, was.SyncOptions):
		return "its sync options"
	case !jsonEqual(app.Retry, was.Retry):
		return "its retry policy"
	}
	return ""
}

func jsonEqual(a, b any) bool {
	x, err1 := canonical(a)
	y, err2 := canonical(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// ArgoFacts are argocd.sync's frozen facts.
type ArgoFacts struct {
	Cluster   string    `json:"cluster,omitempty"`
	App       ArgoApp   `json:"app"`
	CheckedAt time.Time `json:"checked_at"`
}

// ArgoResult is argocd.sync's result: the sync was started; its outcome is Argo CD's.
type ArgoResult struct {
	Namespace string   `json:"namespace"`
	App       string   `json:"app"`
	Revisions []string `json:"revisions"`
	Prune     bool     `json:"prune,omitempty"`
	Observed  bool     `json:"observed,omitempty"`
}

// initiator is the operation's initiatedBy.username, naming the action.
func initiator(actionID string) string { return "mainloop/" + actionID }

// maxArgoShown is how many out-of-sync resources the card lists by name.
const maxArgoShown = 50

// argoExpiry is a delete's expiry for a sync that may prune, since pruning deletes.
func argoExpiry(args any) time.Duration {
	if a, ok := args.(ArgoArgs); ok && a.Prune {
		return DeleteExpiry
	}
	return UndecidedExpiry
}

func normalizeArgoSync(raw json.RawMessage) (any, error) {
	var a ArgoArgs
	if err := decodeStrict(raw, &a); err != nil {
		return nil, err
	}
	if a.Namespace == "" {
		a.Namespace = DefaultArgoNamespace
	}
	switch {
	case !dnsLabelRE.MatchString(a.Namespace):
		return nil, fmt.Errorf("%w: namespace must be a Kubernetes namespace name", ErrInvalid)
	case !dnsSubdomainRE.MatchString(a.App):
		return nil, fmt.Errorf("%w: app must be an Argo CD Application name", ErrInvalid)
	}
	return a, nil
}

func preflightArgoSync(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
	var a ArgoArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if err := s.kubeReady(); err != nil {
		return nil, err
	}
	if s.selfTarget(a.Namespace, "Application", a.App, true) {
		return nil, refuse("namespace %s is the executor's own; it changes only through GitOps", a.Namespace)
	}
	app, err := s.Kube.App(ctx, a.Namespace, a.App)
	if err != nil {
		return nil, badTarget(err)
	}
	switch {
	case !app.Exists:
		return nil, refuse("Application %s/%s was not found", a.Namespace, a.App)
	case app.deploysInto(s.ExecutorNamespace) != "":
		return nil, refuse("Application %s/%s %s; the executor changes only through GitOps the owner merges",
			a.Namespace, a.App, app.deploysInto(s.ExecutorNamespace))
	case app.Operation != "":
		return nil, refuse("Application %s/%s already has a sync %s; request again once it ends", a.Namespace, a.App, strings.ToLower(app.Operation))
	case len(app.Revisions) == 0:
		return nil, refuse("Application %s/%s has no resolved revision yet; Argo CD has not compared it", a.Namespace, a.App)
	}
	return ArgoFacts{Cluster: s.Cluster, App: app, CheckedAt: now}, nil
}

func renderArgoSync(r dbq.PrivilegedAction, ctx map[string]any) (string, string, map[string]any, error) {
	var f ArgoFacts
	var a ArgoArgs
	if json.Unmarshal(r.FrozenFacts, &f) != nil || json.Unmarshal(r.Args, &a) != nil {
		return "", "", nil, errors.New("privileged: argocd.sync facts cannot be read")
	}
	app := f.App
	title := fmt.Sprintf("Sync Argo CD app %s%s", app.Name, onCluster(f.Cluster))
	if a.Prune {
		title += " with prune"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Starts a sync of Application %s/%s at %s, as `argocd app sync --revision` does", app.Namespace, app.Name, revisionsLine(app.Revisions))
	if a.Prune {
		b.WriteString(", deleting the resources no longer in Git")
	}
	b.WriteString(". The sync's outcome is Argo CD's; the result says it started.\n")
	fmt.Fprintf(&b, "Now: %s, %s.\n", orUnknown(app.Sync), strings.ToLower(orUnknown(app.Health)))
	if app.Destination != "" {
		fmt.Fprintf(&b, "It deploys to namespace %s", app.Destination)
		if app.DestinationServer != "" {
			fmt.Fprintf(&b, " on %s", app.DestinationServer)
		}
		b.WriteString(".\n")
	}
	if len(app.Namespaces) > 0 {
		fmt.Fprintf(&b, "Its resources are in namespaces %s.\n", strings.Join(app.Namespaces, ", "))
	}
	if src := sourcesLine(app.Sources); src != "" {
		fmt.Fprintf(&b, "From %s.\n", src)
	}
	if len(app.SyncOptions) > 0 {
		fmt.Fprintf(&b, "Sync options (the app's own): %s.\n", strings.Join(app.SyncOptions, ", "))
	}
	if limit, ok := app.Retry["limit"]; ok {
		fmt.Fprintf(&b, "On failure Argo CD retries it (limit %v).\n", limit)
	} else if app.Retry != nil {
		b.WriteString("On failure Argo CD retries it, per the app's retry policy.\n")
	}
	if len(app.OutOfSync) > 0 {
		fmt.Fprintf(&b, "Out of sync (%d):\n", len(app.OutOfSync))
		for i, res := range app.OutOfSync {
			if i == maxArgoShown {
				fmt.Fprintf(&b, "- and %d more (the audit page lists them all)\n", len(app.OutOfSync)-maxArgoShown)
				break
			}
			fmt.Fprintf(&b, "- %s\n", res)
		}
	} else {
		b.WriteString("Nothing is out of sync: a sync re-applies it and re-runs its hooks.\n")
	}
	b.WriteString("If Git moves to another revision, the app's destination, source or sync policy changes, or other resources go " +
		"out of sync, before this runs, nothing is started.")
	var tags []string
	if a.Prune && slices.ContainsFunc(app.OutOfSync, func(r ArgoResource) bool { return r.Prune }) {
		tags = append(tags, "destructive: prunes resources")
	}
	if app.Automated {
		tags = append(tags, "auto-sync is on: Argo CD would sync it anyway")
	}
	for _, o := range app.SyncOptions {
		switch o {
		case "Replace=true":
			tags = append(tags, "Replace=true: replaces objects rather than applying to them")
		case "Force=true", "Force=true,Replace=true":
			tags = append(tags, "destructive: Force=true deletes and recreates objects it cannot patch")
		}
	}
	tags = append(tags, namespaceTags(app.Destination)...)
	for _, ns := range app.Namespaces {
		if ns != app.Destination {
			tags = append(tags, namespaceTags(ns)...)
		}
	}
	if app.Namespace != DefaultArgoNamespace {
		tags = append(tags, namespaceTags(app.Namespace)...)
	}
	if len(tags) > 0 {
		fmt.Fprintf(&b, "\nRisk: %s.", strings.Join(tags, ", "))
	}
	return title, b.String(), ctx, nil
}

// sourcesLine summarises spec.source or spec.sources: each one's repository, path or chart, and
// target revision, with a note of inline Helm values (bound, not shown).
func sourcesLine(raw json.RawMessage) string {
	var list []map[string]any
	if json.Unmarshal(raw, &list) != nil {
		var one map[string]any
		if json.Unmarshal(raw, &one) != nil || one == nil {
			return ""
		}
		list = []map[string]any{one}
	}
	parts := make([]string, 0, len(list))
	for _, src := range list {
		str := func(k string) string { v, _ := src[k].(string); return v }
		p := str("repoURL")
		if path := str("path"); path != "" {
			p += " path " + path
		}
		if chart := str("chart"); chart != "" {
			p += " chart " + chart
		}
		if rev := str("targetRevision"); rev != "" {
			p += " at " + rev
		}
		if helm, ok := src["helm"].(map[string]any); ok && (helm["values"] != nil || helm["valuesObject"] != nil || helm["parameters"] != nil) {
			p += " (with inline Helm values or parameters)"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}

func revisionsLine(revs []string) string {
	short := make([]string, 0, len(revs))
	for _, r := range revs {
		if len(r) == 40 {
			r = r[:12]
		}
		short = append(short, r)
	}
	if len(short) == 1 {
		return "revision " + short[0]
	}
	return "revisions " + strings.Join(short, ", ")
}

// argoSync runs argocd.sync: it starts a sync at the frozen revisions, only while the Application
// is the one the owner saw, Git has not moved and the same resources are out of sync.
func (r *Runner) argoSync(ctx context.Context, row dbq.PrivilegedAction) outcome {
	var f ArgoFacts
	var a ArgoArgs
	if json.Unmarshal(row.FrozenFacts, &f) != nil || json.Unmarshal(row.Args, &a) != nil {
		return outcome{state: StateFailed, err: "the frozen facts cannot be read"}
	}
	if r.Kube == nil {
		return outcome{state: StateFailed, err: "this executor has no Kubernetes access"}
	}
	was := f.App
	res := ArgoResult{Namespace: was.Namespace, App: was.Name, Revisions: was.Revisions, Prune: a.Prune}
	now, err := r.Kube.App(ctx, was.Namespace, was.Name)
	if err != nil {
		return kubeFailure(err)
	}
	switch {
	case now.Exists && (now.Action == row.ID || now.InitiatedBy == initiator(row.ID)):
		res.Observed = true
		return outcome{state: StateSucceeded, result: res}
	case !now.Exists:
		return outcome{state: StateStale, err: "the Application was deleted since the owner saw it"}
	case now.UID != was.UID:
		return outcome{state: StateStale, err: "the Application was replaced since the owner saw it"}
	case !slices.Equal(now.Revisions, was.Revisions):
		return outcome{state: StateStale, err: fmt.Sprintf("Git moved since the owner saw it (%s, now %s)", revisionsLine(was.Revisions), revisionsLine(now.Revisions))}
	case now.specChange(was) != "":
		return outcome{state: StateStale, err: "the Application's " + strings.TrimPrefix(now.specChange(was), "its ") + " changed since the owner saw it"}
	case now.deploysInto(r.ExecutorNamespace) != "":
		return outcome{state: StateFailed, err: "the Application now " + now.deploysInto(r.ExecutorNamespace) + "; the executor changes only through GitOps"}
	case !slices.Equal(now.OutOfSync, was.OutOfSync):
		return outcome{state: StateStale, err: "other resources are out of sync than the owner saw"}
	case now.Operation != "":
		return retry(fmt.Errorf("a sync is %s; waiting for it to end", strings.ToLower(now.Operation)))
	}
	if err := r.Kube.Sync(ctx, now, a.Prune, row.ID); err != nil {
		return kubeFailure(err) // a concurrent write retries; the next attempt observes
	}
	return outcome{state: StateSucceeded, result: res}
}

// App reads an Application; a missing one has Exists false.
func (k KubeActions) App(ctx context.Context, namespace, name string) (ArgoApp, error) {
	c, err := k.Argo(ctx)
	if err != nil {
		return ArgoApp{}, err
	}
	app := ArgoApp{Namespace: namespace, Name: name}
	o, err := c.Resource(applicationsGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return app, nil
	case err != nil:
		return ArgoApp{}, kubeError(err)
	}
	return ParseArgoApp(o.Object), nil
}

// ParseArgoApp reads the fields argocd.sync uses from an Application object.
func ParseArgoApp(o map[string]any) ArgoApp {
	u := &unstructured.Unstructured{Object: o}
	app := ArgoApp{
		Exists: true, Namespace: u.GetNamespace(), Name: u.GetName(), UID: string(u.GetUID()), ResourceVersion: u.GetResourceVersion(),
		Action: u.GetAnnotations()[AnnotationAction],
	}
	app.Destination, _, _ = unstructured.NestedString(o, "spec", "destination", "namespace")
	if app.DestinationServer, _, _ = unstructured.NestedString(o, "spec", "destination", "name"); app.DestinationServer == "" {
		app.DestinationServer, _, _ = unstructured.NestedString(o, "spec", "destination", "server")
	}
	app.Sync, _, _ = unstructured.NestedString(o, "status", "sync", "status")
	app.Health, _, _ = unstructured.NestedString(o, "status", "health", "status")
	if rev, _, _ := unstructured.NestedString(o, "status", "sync", "revision"); rev != "" {
		app.Revisions = []string{rev}
	}
	if revs, ok, _ := unstructured.NestedStringSlice(o, "status", "sync", "revisions"); ok && len(revs) > 0 {
		app.Revisions, app.MultiSource = revs, true
	}
	_, app.Automated, _ = unstructured.NestedMap(o, "spec", "syncPolicy", "automated")
	app.SyncOptions, _, _ = unstructured.NestedStringSlice(o, "spec", "syncPolicy", "syncOptions")
	if retry, ok, _ := unstructured.NestedMap(o, "spec", "syncPolicy", "retry"); ok {
		app.Retry = retry
	}
	if srcs, ok, _ := unstructured.NestedFieldNoCopy(o, "spec", "sources"); ok {
		app.Sources, _ = canonical(srcs)
	} else if src, ok, _ := unstructured.NestedFieldNoCopy(o, "spec", "source"); ok {
		app.Sources, _ = canonical(src)
	}
	app.InitiatedBy, _, _ = unstructured.NestedString(o, "status", "operationState", "operation", "initiatedBy", "username")
	if _, pending, _ := unstructured.NestedMap(o, "operation"); pending {
		app.Operation = "pending"
	}
	if phase, _, _ := unstructured.NestedString(o, "status", "operationState", "phase"); phase == "Running" || phase == "Terminating" {
		app.Operation = phase
	}
	resources, _, _ := unstructured.NestedSlice(o, "status", "resources")
	namespaces := map[string]bool{}
	for _, raw := range resources {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := m["kind"].(string)
		ns, _ := m["namespace"].(string)
		name, _ := m["name"].(string)
		switch {
		case ns != "":
			namespaces[ns] = true
		case kind == "Namespace":
			namespaces[name] = true
		default:
			app.clusterScoped = append(app.clusterScoped, name)
		}
		status, _ := m["status"].(string)
		prune, _ := m["requiresPruning"].(bool)
		if status != "OutOfSync" && !prune {
			continue
		}
		if g, _ := m["group"].(string); g != "" {
			kind += "." + g
		}
		app.OutOfSync = append(app.OutOfSync, ArgoResource{Kind: kind, Namespace: ns, Name: name, Status: status, Prune: prune})
	}
	sort.Slice(app.OutOfSync, func(i, j int) bool { return app.OutOfSync[i].String() < app.OutOfSync[j].String() })
	for ns := range namespaces {
		app.Namespaces = append(app.Namespaces, ns)
	}
	sort.Strings(app.Namespaces)
	return app
}

// SyncPatch is the merge patch that starts a sync, as the argocd server writes it for `argocd app
// sync --revision`: an operation pinned to the app's revisions, with the app's own sync options
// and retry policy (the controller reads them from the operation, not the spec), at its
// resourceVersion, marked with the action.
func SyncPatch(app ArgoApp, prune bool, actionID string) ([]byte, error) {
	sync := map[string]any{"prune": prune}
	if len(app.SyncOptions) > 0 {
		sync["syncOptions"] = app.SyncOptions
	}
	if app.MultiSource {
		sync["revisions"] = app.Revisions
	} else if len(app.Revisions) == 1 {
		sync["revision"] = app.Revisions[0]
	}
	op := map[string]any{
		"initiatedBy": map[string]any{"username": initiator(actionID)},
		"info":        []map[string]string{{"name": "Reason", "value": "approved Mainloop privileged action " + actionID}},
		"sync":        sync,
	}
	if app.Retry != nil {
		op["retry"] = app.Retry
	}
	return json.Marshal(map[string]any{
		"metadata":  map[string]any{"resourceVersion": app.ResourceVersion, "annotations": map[string]string{AnnotationAction: actionID}},
		"operation": op,
	})
}

// Sync starts a sync of app at its revisions, only at its ResourceVersion.
func (k KubeActions) Sync(ctx context.Context, app ArgoApp, prune bool, actionID string) error {
	c, err := k.Argo(ctx)
	if err != nil {
		return err
	}
	body, err := SyncPatch(app, prune, actionID)
	if err != nil {
		return err
	}
	_, err = c.Resource(applicationsGVR).Namespace(app.Namespace).Patch(ctx, app.Name, types.MergePatchType, body, metav1.PatchOptions{FieldManager: FieldManager})
	switch {
	case apierrors.IsConflict(err) || apierrors.IsNotFound(err):
		return fmt.Errorf("%w: it changed or was deleted meanwhile", ErrTargetChanged)
	case err != nil:
		return kubeError(err)
	}
	return nil
}

func newArgo(c KubeClients) func(ctx context.Context) (dynamic.Interface, error) {
	return func(ctx context.Context) (dynamic.Interface, error) {
		cfg, err := c.Config(ctx, AccountArgo)
		if err != nil {
			return nil, err
		}
		return dynamic.NewForConfig(cfg)
	}
}
