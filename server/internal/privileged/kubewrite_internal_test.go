package privileged

import (
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
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
