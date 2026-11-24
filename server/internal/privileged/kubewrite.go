package privileged

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"
)

// AccountWriter may get, create and patch any object: k8s.patch and k8s.apply. RBAC cannot grant
// "everything but Secrets", so it can read them; the executor refuses Secrets for these types and
// reads nothing else through it.
const AccountWriter = "mainloop-executor-writer"

// ErrFieldConflict is a server-side apply that would take fields another manager owns.
var ErrFieldConflict = errors.New("privileged: fields are managed by another manager")

type writerFunc = func(ctx context.Context) (dynamic.Interface, meta.RESTMapper, error)

func newWriter(c KubeClients) writerFunc {
	return func(ctx context.Context) (dynamic.Interface, meta.RESTMapper, error) {
		cfg, err := c.Config(ctx, AccountWriter)
		if err != nil {
			return nil, nil, err
		}
		dc, err := dynamic.NewForConfig(cfg)
		if err != nil {
			return nil, nil, err
		}
		disc, err := discovery.NewDiscoveryClientForConfig(cfg)
		if err != nil {
			return nil, nil, err
		}
		return dc, restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc)), nil
	}
}

// ResolveObject maps a manifest's apiVersion and kind to its resource. An unknown kind is
// ErrBadTarget.
func (k KubeActions) ResolveObject(ctx context.Context, apiVersion, kind string) (Resource, error) {
	_, mapper, err := k.Writer(ctx)
	if err != nil {
		return Resource{}, err
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return Resource{}, fmt.Errorf("%w: apiVersion %q", ErrBadTarget, apiVersion)
	}
	m, err := mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: kind}, gv.Version)
	if meta.IsNoMatchError(err) {
		return Resource{}, fmt.Errorf("%w: the cluster has no kind %s in %s", ErrBadTarget, kind, apiVersion)
	}
	if err != nil {
		return Resource{}, err
	}
	return Resource{
		Group: m.Resource.Group, Version: m.Resource.Version, Resource: m.Resource.Resource, Kind: m.GroupVersionKind.Kind,
		Namespaced: m.Scope.Name() == meta.RESTScopeNameNamespace,
	}, nil
}

func (k KubeActions) objects(ctx context.Context, r Resource, namespace string) (dynamic.ResourceInterface, error) {
	dc, _, err := k.Writer(ctx)
	if err != nil {
		return nil, err
	}
	gvr := schema.GroupVersionResource{Group: r.Group, Version: r.Version, Resource: r.Resource}
	if r.Namespaced {
		return dc.Resource(gvr).Namespace(namespace), nil
	}
	return dc.Resource(gvr), nil
}

// Get reads a whole object; a missing one is nil.
func (k KubeActions) Get(ctx context.Context, r Resource, namespace, name string) (map[string]any, error) {
	c, err := k.objects(ctx, r, namespace)
	if err != nil {
		return nil, err
	}
	o, err := c.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, kubeError(err)
	}
	return o.Object, nil
}

// Patch patches an object, or with dryRun only says what the API server would store.
func (k KubeActions) Patch(ctx context.Context, r Resource, namespace, name string, pt types.PatchType, body []byte, dryRun bool) (map[string]any, error) {
	c, err := k.objects(ctx, r, namespace)
	if err != nil {
		return nil, err
	}
	o, err := c.Patch(ctx, name, pt, body, metav1.PatchOptions{FieldManager: FieldManager, DryRun: dryRunOpt(dryRun)})
	if err != nil {
		return nil, writeError(err)
	}
	return o.Object, nil
}

// Apply server-side applies one object as FieldManager, or with dryRun only says what the API
// server would store.
func (k KubeActions) Apply(ctx context.Context, r Resource, obj map[string]any, force, dryRun bool) (map[string]any, error) {
	u := &unstructured.Unstructured{Object: obj}
	c, err := k.objects(ctx, r, u.GetNamespace())
	if err != nil {
		return nil, err
	}
	o, err := c.Apply(ctx, u.GetName(), u, metav1.ApplyOptions{FieldManager: FieldManager, Force: force, DryRun: dryRunOpt(dryRun)})
	if err != nil {
		return nil, writeError(err)
	}
	return o.Object, nil
}

func dryRunOpt(dryRun bool) []string {
	if dryRun {
		return []string{metav1.DryRunAll}
	}
	return nil
}

// writeError classifies a write's error: a missing object is ErrGone; a field conflict
// ErrFieldConflict; a resourceVersion conflict ErrTargetChanged; a write the API server rejects as
// invalid ErrBadTarget, with its reason.
func writeError(err error) error {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		s := status.Status()
		switch {
		case apierrors.IsNotFound(err):
			return ErrGone
		case apierrors.IsConflict(err) && s.Details != nil && hasCause(s.Details.Causes, metav1.CauseTypeFieldManagerConflict):
			return fmt.Errorf("%w: %s", ErrFieldConflict, s.Message)
		case apierrors.IsConflict(err):
			return fmt.Errorf("%w: it changed meanwhile", ErrTargetChanged)
		case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsUnsupportedMediaType(err) ||
			s.Code == 422:
			return fmt.Errorf("%w: the API server rejects it: %s", ErrBadTarget, s.Message)
		}
	}
	return kubeError(err)
}

func hasCause(causes []metav1.StatusCause, t metav1.CauseType) bool {
	for _, c := range causes {
		if c.Type == t {
			return true
		}
	}
	return false
}

// ignoredMeta are metadata fields that change without a change to the object, or that the
// executor binds separately (uid).
var ignoredMeta = []string{"managedFields", "resourceVersion", "generation", "uid", "creationTimestamp", "selfLink"}

// comparable is the part of an object a change is judged by: no status, which moves on its own,
// and none of ignoredMeta or the action annotation.
func comparable(o map[string]any) map[string]any {
	if o == nil {
		return nil
	}
	c := runtime.DeepCopyJSON(o)
	delete(c, "status")
	if md, ok := c["metadata"].(map[string]any); ok {
		for _, k := range ignoredMeta {
			delete(md, k)
		}
		if an, ok := md["annotations"].(map[string]any); ok {
			delete(an, AnnotationAction)
			if len(an) == 0 {
				delete(md, "annotations")
			}
		}
	}
	return c
}

// objectDiff is a unified diff of an object's YAML, live against after; empty when they are the
// same. A missing object is empty, so a creation is all additions.
func objectDiff(live, after map[string]any) (string, error) {
	a, err := yamlText(comparable(live))
	if err != nil {
		return "", err
	}
	b, err := yamlText(comparable(after))
	if err != nil {
		return "", err
	}
	if a == b {
		return "", nil
	}
	return difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(a), B: difflib.SplitLines(b), FromFile: "live", ToFile: "after", Context: 3,
	})
}

func yamlText(o map[string]any) (string, error) {
	if o == nil {
		return "", nil
	}
	b, err := yaml.Marshal(o)
	return string(b), err
}

// actionOf is the privileged action an object was last written by.
func actionOf(o map[string]any) string {
	v, _, _ := unstructured.NestedString(o, "metadata", "annotations", AnnotationAction)
	return v
}

func uidOf(o map[string]any) string {
	v, _, _ := unstructured.NestedString(o, "metadata", "uid")
	return v
}

func resourceVersionOf(o map[string]any) string {
	v, _, _ := unstructured.NestedString(o, "metadata", "resourceVersion")
	return v
}

func argoTrackedObject(o map[string]any) bool {
	if o == nil {
		return false
	}
	ann, _, _ := unstructured.NestedStringMap(o, "metadata", "annotations")
	labels, _, _ := unstructured.NestedStringMap(o, "metadata", "labels")
	return argoTracked(metav1.ObjectMeta{Annotations: ann, Labels: labels})
}

func managedByOf(o map[string]any) string {
	v, _, _ := unstructured.NestedString(o, "metadata", "labels", "app.kubernetes.io/managed-by")
	return v
}

// isSecret says whether r is the core Secret kind, which k8s.patch and k8s.apply refuse: a dry-run
// answer would carry its data to the executor and the card.
func isSecret(r Resource) bool {
	return r.Group == "" && strings.EqualFold(r.Kind, "Secret")
}

// generatedPaths collects the JSON pointers at which two dry runs of the same write disagree: a
// differing value, a key only one has, or a list of another length. Below such a pointer nothing
// more is compared.
func generatedPaths(a, b any, path string, out *[]string) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, path)
			return
		}
		keys := make([]string, 0, len(av)+len(bv))
		for k := range av {
			keys = append(keys, k)
		}
		for k := range bv {
			if _, ok := av[k]; !ok {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := path + "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(k)
			x, inA := av[k]
			y, inB := bv[k]
			if inA != inB {
				*out = append(*out, p)
				continue
			}
			generatedPaths(x, y, p, out)
		}
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			*out = append(*out, path)
			return
		}
		for i := range av {
			generatedPaths(av[i], bv[i], path+"/"+strconv.Itoa(i), out)
		}
	default:
		if !reflect.DeepEqual(a, b) {
			*out = append(*out, path)
		}
	}
}

// maskGenerated is a copy of o with GeneratedValue at each pointer whose parent exists, so a value
// the API server makes up afresh reads the same on every run.
func maskGenerated(o map[string]any, pointers []string) map[string]any {
	if o == nil || len(pointers) == 0 {
		return o
	}
	c := runtime.DeepCopyJSON(o)
	for _, p := range pointers {
		parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
		var parent any = c
		for i, raw := range parts {
			key := strings.NewReplacer("~1", "/", "~0", "~").Replace(raw)
			last := i == len(parts)-1
			switch node := parent.(type) {
			case map[string]any:
				if last {
					node[key] = GeneratedValue
				} else {
					parent = node[key]
				}
			case []any:
				n, err := strconv.Atoi(key)
				if err != nil || n < 0 || n >= len(node) {
					parent = nil
					continue
				}
				if last {
					node[n] = GeneratedValue
				} else {
					parent = node[n]
				}
			default:
				parent = nil
			}
			if parent == nil {
				break
			}
		}
	}
	return c
}
