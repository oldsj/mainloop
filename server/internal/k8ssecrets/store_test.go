package k8ssecrets_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oldsj/mainloop/server/internal/k8ssecrets"
	"github.com/oldsj/mainloop/server/internal/k8ssecrets/secretstest"
)

const ns = "kagent"

func TestCreatePublishesImmutableLabelledSecret(t *testing.T) {
	cl := secretstest.New()
	s := k8ssecrets.New(cl, ns)
	uid, err := s.Create(context.Background(), "mainloop-mcp-b1", "mcp", "ml_abc")
	if err != nil || uid == "" {
		t.Fatalf("Create = %q, %v", uid, err)
	}
	sec, err := cl.CoreV1().Secrets(ns).Get(context.Background(), "mainloop-mcp-b1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sec.Immutable == nil || !*sec.Immutable {
		t.Error("Secret is not immutable")
	}
	if sec.Labels[k8ssecrets.LabelActorEgress] != "true" || sec.Labels[k8ssecrets.LabelPurpose] != "mcp" {
		t.Errorf("labels = %v", sec.Labels)
	}
	if got := string(sec.Data[k8ssecrets.DataKey]); got != "Bearer ml_abc" || len(sec.Data) != 1 {
		t.Errorf("data keys = %d, authorization has the wrong shape", len(sec.Data))
	}
	if sec.Type != corev1.SecretTypeOpaque {
		t.Errorf("type = %q", sec.Type)
	}
}

func TestCreateIsIdempotentAndNeverOverwrites(t *testing.T) {
	cl := secretstest.New()
	s := k8ssecrets.New(cl, ns)
	ctx := context.Background()
	first, err := s.Create(ctx, "n", "mcp", "ml_one")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Create(ctx, "n", "mcp", "ml_one")
	if err != nil || again != first {
		t.Errorf("same Secret: uid %q, %v; want %q", again, err, first)
	}
	if _, err := s.Create(ctx, "n", "mcp", "ml_other"); !errors.Is(err, k8ssecrets.ErrMismatch) {
		t.Errorf("different bearer: err = %v, want ErrMismatch", err)
	}
	if _, err := s.Create(ctx, "n", "git-read", "ml_one"); !errors.Is(err, k8ssecrets.ErrMismatch) {
		t.Errorf("different purpose: err = %v, want ErrMismatch", err)
	}
	if cl.UID(ns, "n") != first {
		t.Error("the Secret was replaced")
	}
}

func TestDeleteCarriesUIDPrecondition(t *testing.T) {
	cl := secretstest.New()
	s := k8ssecrets.New(cl, ns)
	ctx := context.Background()
	old, _ := s.Create(ctx, "n", "mcp", "ml_one")
	// The old Secret is replaced under the same name (a new credential): the delete aimed at the
	// old UID must leave the new Secret alone.
	if err := cl.CoreV1().Secrets(ns).Delete(ctx, "n", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	fresh, _ := s.Create(ctx, "n", "mcp", "ml_two")
	if fresh == old {
		t.Fatal("test setup: UIDs are equal")
	}
	if err := s.Delete(ctx, "n", old); err != nil {
		t.Fatalf("Delete with a stale UID = %v, want nil", err)
	}
	if cl.UID(ns, "n") != fresh {
		t.Fatal("a delete aimed at the old UID removed the new Secret")
	}
	if err := s.Delete(ctx, "n", fresh); err != nil {
		t.Fatal(err)
	}
	if cl.UID(ns, "n") != "" {
		t.Error("Secret not deleted")
	}
	// Already gone is success.
	if err := s.Delete(ctx, "n", fresh); err != nil {
		t.Errorf("Delete of a missing Secret = %v", err)
	}
	if err := s.Delete(ctx, "n", ""); err == nil {
		t.Error("Delete without a UID succeeded")
	}
}

func TestLookup(t *testing.T) {
	cl := secretstest.New()
	s := k8ssecrets.New(cl, ns)
	if _, found, err := s.Lookup(context.Background(), "n"); found || err != nil {
		t.Errorf("Lookup of a missing Secret = %v, %v", found, err)
	}
	want, _ := s.Create(context.Background(), "n", "mcp", "ml_one")
	if uid, found, err := s.Lookup(context.Background(), "n"); uid != want || !found || err != nil {
		t.Errorf("Lookup = %q, %v, %v", uid, found, err)
	}
}

// The role grants get, create and delete only; any other verb would be refused in the cluster.
func TestOnlyGetCreateDelete(t *testing.T) {
	cl := secretstest.New()
	s := k8ssecrets.New(cl, ns)
	ctx := context.Background()
	uid, _ := s.Create(ctx, "n", "mcp", "ml_one")
	_, _, _ = s.Lookup(ctx, "n")
	_, _ = s.Create(ctx, "n", "mcp", "ml_one")
	_ = s.Delete(ctx, "n", uid)
	for _, a := range cl.Actions() {
		switch a.GetVerb() {
		case "get", "create", "delete":
		default:
			t.Errorf("verb %q used", a.GetVerb())
		}
	}
}
