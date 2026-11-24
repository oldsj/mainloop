package privileged

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/redact"
)

// The Kubernetes types that change arbitrary objects. The card shows the server dry-run's diff;
// the executor writes only while a fresh dry run still gives that same diff.
const (
	TypePatch = "k8s.patch"
	TypeApply = "k8s.apply"
)

// Bounds of k8s.patch and k8s.apply.
const (
	// MaxChangeBytes bounds a patch or the manifests.
	MaxChangeBytes = 64 << 10
	// MaxApplyObjects bounds the objects in one apply.
	MaxApplyObjects = 50
	// MaxDiffBytes bounds the diffs of one action together. The card shows every byte of them:
	// the diff is the only statement of what changes, so a larger change is split, never cut.
	MaxDiffBytes = 16 << 10
	// GeneratedValue stands for a value the API server generates afresh on each dry run (a Job's
	// controller-uid, a Pod's token volume name, a webhook's timestamp).
	GeneratedValue = "<server-generated>"
)

var patchTypes = map[string]types.PatchType{
	"merge": types.MergePatchType, "strategic": types.StrategicMergePatchType, "json": types.JSONPatchType,
}

// PatchArgs are k8s.patch's arguments. Patch is a JSON merge patch or strategic merge patch
// (an object), or a JSON patch (an array of operations).
type PatchArgs struct {
	Namespace string          `json:"namespace,omitempty"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	PatchType string          `json:"patch_type"`
	Patch     json.RawMessage `json:"patch"`
}

// ApplyArgs are k8s.apply's arguments: YAML or JSON manifests, server-side applied as
// FieldManager. Namespace is the default for namespaced objects that name none.
type ApplyArgs struct {
	Namespace      string `json:"namespace,omitempty"`
	Manifests      string `json:"manifests"`
	ForceConflicts bool   `json:"force_conflicts,omitempty"`
}

// ObjectChange is one object's frozen change.
type ObjectChange struct {
	Resource  Resource `json:"resource"`
	Namespace string   `json:"namespace,omitempty"`
	Name      string   `json:"name"`
	// UID is the live object's; empty when the change creates it.
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resource_version,omitempty"`
	ArgoTracked     bool   `json:"argo_tracked,omitempty"`
	ManagedBy       string `json:"managed_by,omitempty"`
	// Diff is the unified diff of the object's YAML, live against the dry run's answer, without
	// status and the metadata the API server keeps.
	Diff string `json:"diff"`
	// Generated are the JSON pointers at which two dry runs disagreed; the diff and the check
	// before the write show GeneratedValue there.
	Generated []string `json:"generated,omitempty"`
}

func (c ObjectChange) target() string { return objectName(c.Resource, c.Namespace, c.Name) }

// ChangeFacts are k8s.patch's and k8s.apply's frozen facts.
type ChangeFacts struct {
	Cluster string         `json:"cluster,omitempty"`
	Changes []ObjectChange `json:"changes"`
	// Unchanged names the manifests' objects the apply would leave as they are; it skips them.
	Unchanged []string  `json:"unchanged,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// ChangedObject is one object k8s.patch or k8s.apply wrote.
type ChangedObject struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name"`
	Created         bool   `json:"created,omitempty"`
	ResourceVersion string `json:"resource_version,omitempty"`
	// Observed is set when an earlier attempt had already written it.
	Observed bool `json:"observed,omitempty"`
}

// ChangeResult is k8s.patch's and k8s.apply's result.
type ChangeResult struct {
	Objects []ChangedObject `json:"objects"`
}

// Paths a patch may not touch: the executor binds the write to the resourceVersion it observed
// and marks the object with the action, and the diff does not show status or the metadata the
// API server keeps, so a write may not set them unseen.
var protectedPointers = []string{
	"/status", "/metadata/resourceVersion", "/metadata/uid", "/metadata/managedFields", "/metadata/generation",
	"/metadata/creationTimestamp", "/metadata/selfLink", "/metadata/deletionTimestamp", "/metadata/deletionGracePeriodSeconds",
	"/metadata/annotations/mainloop.dev~1privileged-action",
}

// hiddenMeta are the metadata fields the diff does not show.
var hiddenMeta = []string{"resourceVersion", "uid", "managedFields", "generation", "creationTimestamp", "selfLink",
	"deletionTimestamp", "deletionGracePeriodSeconds"}

func normalizePatch(raw json.RawMessage) (any, error) {
	var a PatchArgs
	if err := decodeStrict(raw, &a); err != nil {
		return nil, err
	}
	kind, err := normalizeRef(a.Namespace, a.Kind, a.Name)
	if err != nil {
		return nil, err
	}
	a.Kind = kind
	pt, ok := patchTypes[a.PatchType]
	switch {
	case !ok:
		return nil, fmt.Errorf("%w: patch_type must be merge, strategic or json", ErrInvalid)
	case len(a.Patch) > MaxChangeBytes:
		return nil, fmt.Errorf("%w: patch is over %d KiB", ErrInvalid, MaxChangeBytes>>10)
	}
	var canonical any
	if pt == types.JSONPatchType {
		var ops []map[string]any
		if err := json.Unmarshal(a.Patch, &ops); err != nil || len(ops) == 0 {
			return nil, fmt.Errorf("%w: a json patch is a non-empty array of operations", ErrInvalid)
		}
		for _, op := range ops {
			if err := checkOperation(op); err != nil {
				return nil, err
			}
		}
		if at := requestCredential(ops); at != "" {
			return nil, fmt.Errorf("%w: the patch sets what looks like a credential at %s (contains_secret); %s", ErrInvalid, at, secretAdvice)
		}
		canonical = ops
	} else {
		var m map[string]any
		if err := json.Unmarshal(a.Patch, &m); err != nil || len(m) == 0 {
			return nil, fmt.Errorf("%w: a %s patch is a non-empty JSON object", ErrInvalid, a.PatchType)
		}
		if field := protectedField(m, false); field != "" {
			return nil, fmt.Errorf("%w: the patch may not set %s: the card's diff does not show it, or the executor binds it", ErrInvalid, field)
		}
		if at := requestCredential(m); at != "" {
			return nil, fmt.Errorf("%w: the patch sets what looks like a credential at %s (contains_secret); %s", ErrInvalid, at, secretAdvice)
		}
		canonical = m
	}
	if a.Patch, err = json.Marshal(canonical); err != nil {
		return nil, err
	}
	return a, nil
}

func checkOperation(op map[string]any) error {
	name, _ := op["op"].(string)
	if !slices.Contains([]string{"add", "remove", "replace", "move", "copy"}, name) {
		// test is refused: refused or not, it would tell the agent whether a guess about a field is
		// right without anyone approving. The executor's own check binds the write to the diff.
		return fmt.Errorf("%w: each json patch operation has an op: add, remove, replace, move or copy (not test)", ErrInvalid)
	}
	for _, field := range []string{"path", "from"} {
		p, ok := op[field].(string)
		if field == "from" && !ok && op[field] == nil {
			continue
		}
		if !ok || !strings.HasPrefix(p, "/") || p == "/metadata" {
			return fmt.Errorf("%w: each json patch operation's %s is a JSON pointer below the object's root, not /metadata", ErrInvalid, field)
		}
		for _, protected := range protectedPointers {
			if p == protected || strings.HasPrefix(p, protected+"/") {
				return fmt.Errorf("%w: the patch may not touch %s", ErrInvalid, protected)
			}
		}
	}
	return nil
}

// protectedField names a field of a merge patch or manifest that the diff would not show or the
// executor binds: status, server-kept metadata, the action annotation, or metadata or annotations
// that are not objects. A manifest may carry an empty status and a null creationTimestamp, as
// generated YAML does; they write nothing.
func protectedField(m map[string]any, manifest bool) string {
	if st, ok := m["status"]; ok && (!manifest || !empty(st)) {
		return "status"
	}
	raw, ok := m["metadata"]
	if !ok {
		return ""
	}
	md, isMap := raw.(map[string]any)
	if !isMap {
		return "metadata (other than as an object)"
	}
	for _, k := range hiddenMeta {
		if v, ok := md[k]; ok && (!manifest || k != "creationTimestamp" || v != nil) {
			return "metadata." + k
		}
	}
	if raw, ok := md["annotations"]; ok {
		ann, isMap := raw.(map[string]any)
		switch {
		case !isMap:
			return "metadata.annotations (other than as an object; remove annotations one by one)"
		}
		if _, ok := ann[AnnotationAction]; ok {
			return "the " + AnnotationAction + " annotation"
		}
	}
	return ""
}

func empty(v any) bool {
	m, ok := v.(map[string]any)
	return v == nil || (ok && len(m) == 0)
}

func normalizeApply(raw json.RawMessage) (any, error) {
	var a ApplyArgs
	if err := decodeStrict(raw, &a); err != nil {
		return nil, err
	}
	a.Manifests = strings.TrimSpace(a.Manifests)
	switch {
	case a.Manifests == "":
		return nil, fmt.Errorf("%w: manifests are YAML or JSON objects, separated by ---", ErrInvalid)
	case len(a.Manifests) > MaxChangeBytes:
		return nil, fmt.Errorf("%w: manifests are over %d KiB", ErrInvalid, MaxChangeBytes>>10)
	case a.Namespace != "" && !dnsLabelRE.MatchString(a.Namespace):
		return nil, fmt.Errorf("%w: namespace must be a Kubernetes namespace name", ErrInvalid)
	}
	objs, err := parseManifests(a.Manifests)
	if err != nil {
		return nil, err
	}
	for i, o := range objs {
		if ns := (&unstructured.Unstructured{Object: o}).GetNamespace(); ns != "" && a.Namespace != "" && ns != a.Namespace {
			return nil, fmt.Errorf("%w: an object names namespace %s, not %s", ErrInvalid, ns, a.Namespace)
		}
		if at := requestCredential(o); at != "" {
			return nil, fmt.Errorf("%w: object %d of the manifests sets what looks like a credential at %s (contains_secret); %s",
				ErrInvalid, i+1, at, secretAdvice)
		}
	}
	if redact.Credential(a.Manifests) {
		return nil, fmt.Errorf("%w: the manifests hold what looks like a credential (contains_secret); %s", ErrInvalid, secretAdvice)
	}
	return a, nil
}

// parseManifests reads YAML or JSON objects. Each names its apiVersion, kind and name; none sets
// what the executor binds (resourceVersion, uid, the action annotation); no object appears twice.
func parseManifests(s string) ([]map[string]any, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(s), 4096)
	var out []map[string]any
	seen := map[string]bool{}
	for {
		var o map[string]any
		if err := dec.Decode(&o); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("%w: manifests: %s", ErrInvalid, err.Error())
		}
		if len(o) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: o}
		gv, err := schema.ParseGroupVersion(u.GetAPIVersion())
		switch {
		case err != nil || u.GetAPIVersion() == "" || u.GetKind() == "":
			return nil, fmt.Errorf("%w: every manifest has an apiVersion and a kind", ErrInvalid)
		case strings.HasSuffix(u.GetKind(), "List"):
			return nil, fmt.Errorf("%w: give the objects of a %s one by one", ErrInvalid, u.GetKind())
		case !validObjectName(u.GetName()) || u.GetGenerateName() != "":
			return nil, fmt.Errorf("%w: %s needs metadata.name (generateName is not supported)", ErrInvalid, u.GetKind())
		case u.GetNamespace() != "" && !dnsLabelRE.MatchString(u.GetNamespace()):
			return nil, fmt.Errorf("%w: %s %s: metadata.namespace must be a Kubernetes namespace name", ErrInvalid, u.GetKind(), u.GetName())
		case protectedField(o, true) != "":
			return nil, fmt.Errorf("%w: %s %s sets %s: the card's diff does not show it, or the executor binds it", ErrInvalid, u.GetKind(), u.GetName(), protectedField(o, true))
		}
		key := gv.Group + "/" + u.GetKind() + "/" + u.GetNamespace() + "/" + u.GetName()
		if seen[key] {
			return nil, fmt.Errorf("%w: %s %s appears twice", ErrInvalid, u.GetKind(), u.GetName())
		}
		seen[key] = true
		out = append(out, o)
	}
	switch {
	case len(out) == 0:
		return nil, fmt.Errorf("%w: manifests hold no object", ErrInvalid)
	case len(out) > MaxApplyObjects:
		return nil, fmt.Errorf("%w: at most %d objects per apply", ErrInvalid, MaxApplyObjects)
	}
	return out, nil
}

// placed is a manifest's object resolved and given its namespace.
type placed struct {
	r   Resource
	obj map[string]any
}

func (p placed) namespace() string { return (&unstructured.Unstructured{Object: p.obj}).GetNamespace() }
func (p placed) name() string      { return (&unstructured.Unstructured{Object: p.obj}).GetName() }

// place resolves each manifest's kind and sets a namespaced object's namespace to the default.
// A kind the cluster lacks, or an object the executor may not change, is a refusal.
func place(ctx context.Context, s *Service, a ApplyArgs) ([]placed, error) {
	objs, err := parseManifests(a.Manifests)
	if err != nil {
		return nil, err
	}
	out := make([]placed, 0, len(objs))
	for _, o := range objs {
		u := &unstructured.Unstructured{Object: runtime.DeepCopyJSON(o)}
		r, err := s.Kube.ResolveObject(ctx, u.GetAPIVersion(), u.GetKind())
		if err != nil {
			return nil, badTarget(err)
		}
		ns := u.GetNamespace()
		if r.Namespaced && ns == "" {
			if ns = a.Namespace; ns == "" {
				return nil, refuse("%s %s is namespaced; give its namespace in the manifest or in namespace", r.Qualified(), u.GetName())
			}
			u.SetNamespace(ns)
		}
		if err := s.checkTarget(r, ns, u.GetName()); err != nil {
			return nil, err
		}
		out = append(out, placed{r: r, obj: u.Object})
	}
	return out, nil
}

// checkTarget refuses a target k8s.patch and k8s.apply must not change: one in the wrong scope, a
// Secret, or the executor's own.
func (s *Service) checkTarget(r Resource, namespace, name string) error {
	switch {
	case r.Namespaced && namespace == "":
		return refuse("%s is namespaced; give its namespace", r.Qualified())
	case !r.Namespaced && namespace != "":
		return refuse("%s is cluster-scoped; leave its namespace empty", r.Qualified())
	case isSecret(r):
		return refuse("Secrets are refused here: a dry run would read the value. Use onepassword.item_to_secret")
	case s.selfTarget(namespace, r.Kind, name, r.Namespaced):
		return refuse("%s is the executor's own; it changes only through GitOps", objectName(r, namespace, name))
	}
	return nil
}

// changeRefusal turns a dry run's error into a refusal where the request cannot work as asked.
func changeRefusal(target string, err error) error {
	switch {
	case errors.Is(err, ErrGone):
		return refuse("%s was not found", target)
	case errors.Is(err, ErrFieldConflict):
		return refuse("%s: %s; set force_conflicts to take them", target, strings.TrimPrefix(err.Error(), ErrFieldConflict.Error()+": "))
	case errors.Is(err, ErrBadTarget):
		return refuse("%s: %s", target, strings.TrimPrefix(err.Error(), ErrBadTarget.Error()+": "))
	}
	return badTarget(err)
}

func newChange(r Resource, namespace, name string, live map[string]any, diff string) ObjectChange {
	return ObjectChange{
		Resource: r, Namespace: namespace, Name: name, UID: uidOf(live), ResourceVersion: resourceVersionOf(live),
		ArgoTracked: argoTrackedObject(live), ManagedBy: managedByOf(live), Diff: diff,
	}
}

func preflightPatch(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
	var a PatchArgs
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
	if err := s.checkTarget(r, a.Namespace, a.Name); err != nil {
		return nil, err
	}
	target := objectName(r, a.Namespace, a.Name)
	live, err := s.Kube.Get(ctx, r, a.Namespace, a.Name)
	if err != nil {
		return nil, badTarget(err)
	}
	if live == nil {
		return nil, refuse("%s was not found", target)
	}
	sets, err := patchPointers(patchTypes[a.PatchType], a.Patch)
	if err != nil {
		return nil, err
	}
	c, err := dryRunChange(r, a.Namespace, a.Name, live, sets, func() (map[string]any, error) {
		return s.Kube.Patch(ctx, r, a.Namespace, a.Name, patchTypes[a.PatchType], a.Patch, true)
	})
	switch {
	case err != nil:
		return nil, changeRefusal(target, err)
	case c.Diff == "":
		return nil, refuse("the patch changes nothing in %s (status and server-kept metadata aside)", target)
	}
	f := ChangeFacts{Cluster: s.Cluster, Changes: []ObjectChange{c}, CheckedAt: now}
	return f, f.check()
}

// dryRunChange runs a dry run twice and diffs the object against its answer, with the values on
// which the two answers disagree shown as GeneratedValue: the API server made them up, and the
// check before the write must not trip on them. A value the request itself sets is never masked:
// if it differs between the runs, the request is refused. So is a diff that shows what looks like
// a credential.
func dryRunChange(r Resource, namespace, name string, live map[string]any, sets []string, dryRun func() (map[string]any, error)) (ObjectChange, error) {
	first, err := dryRun()
	if err != nil {
		return ObjectChange{}, err
	}
	second, err := dryRun()
	if err != nil {
		return ObjectChange{}, err
	}
	var gen []string
	generatedPaths(comparable(first), comparable(second), "", &gen)
	target := objectName(r, namespace, name)
	for _, g := range gen {
		for _, p := range sets {
			if g == p || strings.HasPrefix(g, p+"/") || strings.HasPrefix(p, g+"/") {
				return ObjectChange{}, refuse("%s: the value at %s, which the request sets, came out differently on two dry runs: "+
					"the API server generates it (or adds to it) on each write, so the request cannot set it; leave it out", target, g)
			}
		}
	}
	after := maskGenerated(first, gen)
	diff, err := objectDiff(live, after)
	if err != nil {
		return ObjectChange{}, err
	}
	// The walk names the field; the diff's text catches a value YAML folded or escaped there, which
	// the walk cannot match to the diff.
	if at := credentialAt(diff, comparable(live), comparable(after)); at != "" {
		return ObjectChange{}, refuse("the change to %s shows what looks like a credential at %s (contains_secret); %s", target, at, secretAdvice)
	}
	if redact.Credential(diff) {
		return ObjectChange{}, refuse("the change to %s shows what looks like a credential in its diff (contains_secret); %s", target, secretAdvice)
	}
	c := newChange(r, namespace, name, live, diff)
	c.Generated = gen
	return c, nil
}

// check refuses facts the card cannot show whole: the card is the owner's only view of the change.
func (f ChangeFacts) check() error {
	total := 0
	for _, c := range f.Changes {
		total += len(c.Diff)
	}
	if total > MaxDiffBytes {
		// No size: it would tell the agent how much a guess changed the diff.
		return refuse("the change as a diff is over the %d KiB a card shows whole; split it", MaxDiffBytes>>10)
	}
	return nil
}

func preflightApply(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
	var a ApplyArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if err := s.kubeReady(); err != nil {
		return nil, err
	}
	objs, err := place(ctx, s, a)
	if err != nil {
		return nil, err
	}
	f := ChangeFacts{Cluster: s.Cluster, CheckedAt: now}
	for _, p := range objs {
		target := objectName(p.r, p.namespace(), p.name())
		live, err := s.Kube.Get(ctx, p.r, p.namespace(), p.name())
		if err != nil {
			return nil, badTarget(err)
		}
		var sets []string
		leafPointers(p.obj, "", &sets)
		c, err := dryRunChange(p.r, p.namespace(), p.name(), live, sets, func() (map[string]any, error) {
			return s.Kube.Apply(ctx, p.r, p.obj, a.ForceConflicts, true)
		})
		switch {
		case err != nil:
			return nil, changeRefusal(target, err)
		case c.Diff == "":
			f.Unchanged = append(f.Unchanged, target)
			continue
		}
		f.Changes = append(f.Changes, c)
	}
	if len(f.Changes) == 0 {
		return nil, refuse("the manifests change nothing (status and server-kept metadata aside)")
	}
	return f, f.check()
}

func renderChange(r dbq.PrivilegedAction, ctx map[string]any) (string, string, map[string]any, error) {
	var f ChangeFacts
	if err := json.Unmarshal(r.FrozenFacts, &f); err != nil {
		return "", "", nil, err
	}
	var b strings.Builder
	var title string
	var tags []string
	if r.Type == TypePatch {
		var a PatchArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return "", "", nil, err
		}
		title = "Patch " + f.Changes[0].target() + onCluster(f.Cluster)
		fmt.Fprintf(&b, "A %s patch, as `kubectl patch --type %s` does. The diff is the API server's dry run against the live object:\n", a.PatchType, a.PatchType)
	} else {
		var a ApplyArgs
		if err := json.Unmarshal(r.Args, &a); err != nil {
			return "", "", nil, err
		}
		switch c := f.Changes[0]; {
		case len(f.Changes) > 1:
			title = fmt.Sprintf("Apply %d objects%s", len(f.Changes), onCluster(f.Cluster))
		case c.UID == "":
			title = "Create " + c.target() + onCluster(f.Cluster)
		default:
			title = "Apply " + c.target() + onCluster(f.Cluster)
		}
		fmt.Fprintf(&b, "Server-side apply as %s, as `kubectl apply --server-side` does. Each diff is the API server's dry run against the live object:\n", FieldManager)
		if a.ForceConflicts {
			tags = append(tags, "force_conflicts: takes fields other managers own")
		}
	}
	created := 0
	for _, c := range f.Changes {
		if c.UID == "" {
			created++
			fmt.Fprintf(&b, "\n%s (new):\n", c.target())
		} else {
			fmt.Fprintf(&b, "\n%s (uid %s):\n", c.target(), c.UID)
		}
		b.WriteString("```diff\n" + strings.TrimSuffix(c.Diff, "\n") + "\n```\n")
		if len(c.Generated) > 0 {
			fmt.Fprintf(&b, "%s marks values the API server generates on each run (%s).\n", GeneratedValue, strings.Join(c.Generated, ", "))
		}
		tags = append(tags, changeTags(c)...)
	}
	if len(f.Unchanged) > 0 {
		fmt.Fprintf(&b, "\nLeft as they are: %s.\n", strings.Join(f.Unchanged, ", "))
	}
	b.WriteString("\nStatus is not shown. If an object is replaced, or would change differently, before this runs, nothing more is written.")
	if created > 0 {
		tags = append(tags, fmt.Sprintf("creates %d object(s)", created))
	}
	if tags = dedupe(tags); len(tags) > 0 {
		fmt.Fprintf(&b, "\nRisk: %s.", strings.Join(tags, ", "))
	}
	if ns := f.Changes[0].Namespace; ns != "" && !slices.ContainsFunc(f.Changes, func(c ObjectChange) bool { return c.Namespace != ns }) {
		ctx["namespace"] = ns
	}
	return title, b.String(), ctx, nil
}

func changeTags(c ObjectChange) []string {
	var tags []string
	ns := c.Namespace
	switch {
	case c.Resource.Group == "" && c.Resource.Kind == "Namespace":
		ns = c.Name
	case !c.Resource.Namespaced:
		tags = append(tags, "cluster-scoped: "+c.Resource.Qualified())
	}
	tags = append(tags, namespaceTags(ns)...)
	if c.Resource.Group == "rbac.authorization.k8s.io" {
		tags = append(tags, "changes access rights")
	}
	if c.Resource.Kind == "CustomResourceDefinition" {
		tags = append(tags, "changes an API every object of it uses")
	}
	if c.ArgoTracked {
		tags = append(tags, "Argo-managed: Argo CD may revert it, or it drifts from Git")
	}
	if c.ManagedBy != "" {
		tags = append(tags, "managed by "+c.ManagedBy)
	}
	return tags
}

func dedupe(s []string) []string {
	var out []string
	for _, v := range s {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// boundPatch is the patch with the executor's preconditions: the resourceVersion it observed and
// the action annotation. after is the dry run's answer, so a JSON patch knows whether annotations
// exist after the requester's operations.
func boundPatch(pt types.PatchType, patch []byte, rv, actionID string, after map[string]any) ([]byte, error) {
	if pt == types.JSONPatchType {
		var ops []any
		if err := json.Unmarshal(patch, &ops); err != nil {
			return nil, err
		}
		if _, ok, _ := unstructured.NestedMap(after, "metadata", "annotations"); ok {
			ops = append(ops, map[string]any{"op": "add", "path": "/metadata/annotations/mainloop.dev~1privileged-action", "value": actionID})
		} else {
			ops = append(ops, map[string]any{"op": "add", "path": "/metadata/annotations", "value": map[string]any{AnnotationAction: actionID}})
		}
		ops = append(ops, map[string]any{"op": "add", "path": "/metadata/resourceVersion", "value": rv})
		return json.Marshal(ops)
	}
	var m map[string]any
	if err := json.Unmarshal(patch, &m); err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(m, rv, "metadata", "resourceVersion"); err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(m, actionID, "metadata", "annotations", AnnotationAction); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// change runs k8s.patch and k8s.apply. Before any write, every object is checked: the same object
// the owner saw (or still absent, for a creation), and a fresh dry run gives the diff the owner
// approved. Then each is written with the resourceVersion just observed; a concurrent write retries
// through a new check.
//
// A patched object carries the action annotation, so a later attempt sees its own write. An applied
// one does not (another manager's annotation would conflict): on a later attempt an object whose
// approved diff is now empty is this action's own earlier write. On the first attempt it is stale:
// someone else made the change.
func (r *Runner) change(ctx context.Context, row dbq.PrivilegedAction) outcome {
	var f ChangeFacts
	if err := json.Unmarshal(row.FrozenFacts, &f); err != nil || len(f.Changes) == 0 {
		return outcome{state: StateFailed, err: "the frozen facts cannot be read"}
	}
	if r.Kube == nil {
		return outcome{state: StateFailed, err: "this executor has no Kubernetes access"}
	}
	var (
		pa      PatchArgs
		aa      ApplyArgs
		objects = map[string]placed{}
	)
	patch := row.Type == TypePatch
	if patch {
		if err := json.Unmarshal(row.Args, &pa); err != nil {
			return outcome{state: StateFailed, err: "the arguments cannot be read"}
		}
	} else {
		if err := json.Unmarshal(row.Args, &aa); err != nil {
			return outcome{state: StateFailed, err: "the arguments cannot be read"}
		}
		placed, err := place(ctx, r.Service, aa)
		var refused *refusal
		switch {
		case errors.As(err, &refused):
			return changeStale(err, ChangeResult{})
		case err != nil:
			return kubeFailure(err) // a token, discovery or network failure: retry
		}
		for _, p := range placed {
			objects[objectName(p.r, p.namespace(), p.name())] = p
		}
	}
	first := row.Attempts == 1
	// res is what this attempt found written or wrote; every outcome carries it.
	var res ChangeResult
	ended := func(o outcome) outcome {
		if len(res.Objects) > 0 {
			o.result = res
		}
		return o
	}
	type write struct {
		c     ObjectChange
		live  map[string]any
		after map[string]any
	}
	var writes []write
	for _, c := range f.Changes {
		done := ChangedObject{Kind: c.Resource.Qualified(), Namespace: c.Namespace, Name: c.Name, Created: c.UID == ""}
		live, err := r.Kube.Get(ctx, c.Resource, c.Namespace, c.Name)
		if err != nil {
			return ended(kubeFailure(err))
		}
		if live != nil && patch && actionOf(live) == row.ID {
			done.ResourceVersion, done.Observed = resourceVersionOf(live), true
			res.Objects = append(res.Objects, done)
			continue
		}
		switch {
		case c.UID == "" && live != nil && first:
			return changeStale(fmt.Errorf("%s was created by someone else since the owner saw it", c.target()), res)
		case c.UID != "" && live == nil:
			return changeStale(fmt.Errorf("%s was deleted since the owner saw it", c.target()), res)
		case c.UID != "" && uidOf(live) != c.UID:
			return changeStale(fmt.Errorf("%s was replaced by a new object since the owner saw it", c.target()), res)
		}
		var after map[string]any
		if patch {
			after, err = r.Kube.Patch(ctx, c.Resource, c.Namespace, c.Name, patchTypes[pa.PatchType], pa.Patch, true)
		} else {
			p, ok := objects[c.target()]
			if !ok {
				return ended(outcome{state: StateFailed, err: c.target() + " is no longer in the manifests"})
			}
			after, err = r.Kube.Apply(ctx, c.Resource, p.obj, aa.ForceConflicts, true)
		}
		if err != nil {
			if errors.Is(err, ErrGone) || errors.Is(err, ErrFieldConflict) || errors.Is(err, ErrBadTarget) {
				return changeStale(fmt.Errorf("%s: %w", c.target(), err), res)
			}
			return ended(kubeFailure(err))
		}
		diff, err := objectDiff(live, maskGenerated(after, c.Generated))
		if err != nil {
			return ended(outcome{state: StateFailed, err: safeError(err)})
		}
		if diff == "" && !patch && !first {
			done.ResourceVersion, done.Observed = resourceVersionOf(live), true
			res.Objects = append(res.Objects, done)
			continue
		}
		if diff != c.Diff {
			return changeStale(fmt.Errorf("%s changed since the owner saw it: the change would now differ from the approved diff", c.target()), res)
		}
		writes = append(writes, write{c: c, live: live, after: after})
	}
	for _, w := range writes {
		c := w.c
		rv := resourceVersionOf(w.live)
		var (
			out map[string]any
			err error
		)
		if patch {
			var body []byte
			if body, err = boundPatch(patchTypes[pa.PatchType], pa.Patch, rv, row.ID, w.after); err != nil {
				return ended(outcome{state: StateFailed, err: safeError(err)})
			}
			out, err = r.Kube.Patch(ctx, c.Resource, c.Namespace, c.Name, patchTypes[pa.PatchType], body, false)
		} else {
			obj := runtime.DeepCopyJSON(objects[c.target()].obj)
			if rv != "" {
				if err = unstructured.SetNestedField(obj, rv, "metadata", "resourceVersion"); err != nil {
					return ended(outcome{state: StateFailed, err: safeError(err)})
				}
			}
			out, err = r.Kube.Apply(ctx, c.Resource, obj, aa.ForceConflicts, false)
		}
		switch {
		case errors.Is(err, ErrBadTarget) || errors.Is(err, ErrFieldConflict):
			// The API server refused the write itself (an admission webhook that skips dry runs,
			// a quota): it will refuse it again.
			return ended(outcome{state: StateFailed, err: safeError(fmt.Errorf("%s: %w", c.target(), err))})
		case err != nil:
			return ended(kubeFailure(err)) // a concurrent write retries; the next attempt checks again
		}
		res.Objects = append(res.Objects, ChangedObject{
			Kind: c.Resource.Qualified(), Namespace: c.Namespace, Name: c.Name, Created: c.UID == "", ResourceVersion: resourceVersionOf(out),
		})
	}
	return outcome{state: StateSucceeded, result: res}
}

// changeStale ends the action stale, with what an earlier attempt already wrote as its result.
func changeStale(err error, res ChangeResult) outcome {
	msg := strings.TrimPrefix(safeError(err), "privileged: ")
	var r *refusal
	if errors.As(err, &r) {
		msg = r.msg
	}
	o := outcome{state: StateStale, err: msg}
	if len(res.Objects) > 0 {
		names := make([]string, 0, len(res.Objects))
		for _, w := range res.Objects {
			names = append(names, w.Kind+" "+strings.TrimPrefix(w.Namespace+"/"+w.Name, "/"))
		}
		o.err += "; already written: " + strings.Join(names, ", ")
		o.result = res
	}
	return o
}

// AgentFacts is what the requesting session may see of an action's frozen facts. For k8s.patch
// and k8s.apply the diffs are the objects' contents, read with the executor's credential before
// anyone approved, so the session gets each object's name only: not the diff, nor its size or
// hash, which would let it confirm a guess. The owner sees the diff on the card and the audit page.
func AgentFacts(typ string, facts json.RawMessage) json.RawMessage {
	if typ == TypeKubectl && len(facts) > 0 {
		return kubectlAgentFacts(facts)
	}
	if typ == TypeArgoSync && len(facts) > 0 {
		var f ArgoFacts
		if err := json.Unmarshal(facts, &f); err != nil {
			return nil
		}
		f.App.Sources = nil // inline Helm values can hold anything; the card summarises the sources
		b, err := json.Marshal(f)
		if err != nil {
			return nil
		}
		return b
	}
	if typ != TypePatch && typ != TypeApply || len(facts) == 0 {
		return facts
	}
	var f ChangeFacts
	if err := json.Unmarshal(facts, &f); err != nil {
		return nil
	}
	type change struct {
		Object  string `json:"object"`
		UID     string `json:"uid,omitempty"`
		Creates bool   `json:"creates,omitempty"`
	}
	out := struct {
		Cluster   string    `json:"cluster,omitempty"`
		Changes   []change  `json:"changes"`
		Unchanged []string  `json:"unchanged,omitempty"`
		Note      string    `json:"note"`
		CheckedAt time.Time `json:"checked_at"`
	}{Cluster: f.Cluster, Unchanged: f.Unchanged, CheckedAt: f.CheckedAt, Note: "the diffs are on the owner's card only"}
	for _, c := range f.Changes {
		out.Changes = append(out.Changes, change{Object: c.target(), UID: c.UID, Creates: c.UID == ""})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}
