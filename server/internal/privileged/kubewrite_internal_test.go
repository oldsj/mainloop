package privileged

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// The API server's answers map to what the runner does with them: a field-manager conflict is not a
// resourceVersion conflict, and an invalid write is a refusal with the server's reason.
func TestWriteErrorClassifies(t *testing.T) {
	gr := schema.GroupResource{Resource: "configmaps"}
	fieldConflict := apierrors.NewApplyConflict([]metav1.StatusCause{{Type: metav1.CauseTypeFieldManagerConflict, Field: ".data.mode"}}, `conflict with "helm"`)
	for _, tc := range []struct {
		err  error
		want error
	}{
		{apierrors.NewNotFound(gr, "x"), ErrGone},
		{fieldConflict, ErrFieldConflict},
		{apierrors.NewConflict(gr, "x", errors.New("the object has been modified")), ErrTargetChanged},
		{apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, "x", field.ErrorList{field.Invalid(field.NewPath("data"), 1, "bad")}), ErrBadTarget},
		{apierrors.NewForbidden(gr, "x", errors.New("no")), ErrForbidden},
	} {
		if got := writeError(tc.err); !errors.Is(got, tc.want) {
			t.Errorf("writeError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// A diff ignores status and the metadata the API server keeps, so their churn is no change.
func TestObjectDiffIgnoresServerKeptFields(t *testing.T) {
	live := map[string]any{"metadata": map[string]any{"name": "x", "resourceVersion": "1", "uid": "u", "generation": int64(1),
		"managedFields": []any{}, "annotations": map[string]any{AnnotationAction: "old"}}, "data": map[string]any{"a": "b"}, "status": map[string]any{"n": int64(1)}}
	after := map[string]any{"metadata": map[string]any{"name": "x", "resourceVersion": "9", "uid": "u", "generation": int64(2)},
		"data": map[string]any{"a": "b"}, "status": map[string]any{"n": int64(2)}}
	if d, err := objectDiff(live, after); err != nil || d != "" {
		t.Fatalf("diff = %q, %v", d, err)
	}
	after["data"] = map[string]any{"a": "c"}
	d, err := objectDiff(live, after)
	if err != nil || !strings.Contains(d, "-  a: b") || !strings.Contains(d, "+  a: c") || strings.Contains(d, "resourceVersion") {
		t.Fatalf("diff = %q, %v", d, err)
	}
	if d, _ := objectDiff(nil, after); !strings.HasPrefix(d, "--- live\n+++ after\n") || strings.Contains(d, "\n-") {
		t.Fatalf("creation diff = %q", d)
	}
}

// Two dry-run answers that differ in a value, a key and a list length mark exactly those
// pointers, and masking writes the marker at each.
func TestGeneratedPathsAndMask(t *testing.T) {
	a := map[string]any{"metadata": map[string]any{"labels": map[string]any{"batch.kubernetes.io/controller-uid": "u1", "app": "x"}},
		"spec": map[string]any{"volumes": []any{map[string]any{"name": "kube-api-access-abcde"}}, "ports": []any{int64(1)}}}
	b := map[string]any{"metadata": map[string]any{"labels": map[string]any{"batch.kubernetes.io/controller-uid": "u2", "app": "x"}, "extra": "y"},
		"spec": map[string]any{"volumes": []any{map[string]any{"name": "kube-api-access-fghij"}}, "ports": []any{int64(1), int64(2)}}}
	var got []string
	generatedPaths(a, b, "", &got)
	want := []string{"/metadata/extra", "/metadata/labels/batch.kubernetes.io~1controller-uid", "/spec/ports", "/spec/volumes/0/name"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("paths = %v", got)
	}
	m := maskGenerated(a, got)
	if m["metadata"].(map[string]any)["labels"].(map[string]any)["batch.kubernetes.io/controller-uid"] != GeneratedValue ||
		m["spec"].(map[string]any)["volumes"].([]any)[0].(map[string]any)["name"] != GeneratedValue ||
		m["spec"].(map[string]any)["ports"] != GeneratedValue || m["metadata"].(map[string]any)["extra"] != GeneratedValue {
		t.Fatalf("masked = %v", m)
	}
	if a["metadata"].(map[string]any)["labels"].(map[string]any)["batch.kubernetes.io/controller-uid"] != "u1" {
		t.Fatal("masking changed its input")
	}
}

// A delete of a CustomResourceDefinition or an APIService says on the card what goes with it.
func TestDeleteCardTagsWhatGoesWithIt(t *testing.T) {
	for _, tc := range []struct {
		r    Resource
		name string
		want string
	}{
		{Resource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions", Kind: "CustomResourceDefinition"},
			"widgets.example.com", "deletes every widgets.example.com object in the cluster, in every namespace"},
		{Resource{Group: "apiregistration.k8s.io", Version: "v1", Resource: "apiservices", Kind: "APIService"},
			"v1beta1.metrics.k8s.io", "the API it serves (v1beta1.metrics.k8s.io) goes away for every client and controller"},
	} {
		facts, err := json.Marshal(DeleteFacts{Cluster: "test-cluster", Resource: tc.r, Name: tc.name, Object: ObjectMeta{Exists: true, UID: "u1"}})
		if err != nil {
			t.Fatal(err)
		}
		_, body, _, err := renderDelete(dbq.PrivilegedAction{Type: TypeDelete, FrozenFacts: facts}, map[string]any{})
		if err != nil || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %v\n%s", tc.r.Kind, err, body)
		}
	}
}
