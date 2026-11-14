package k8ssecrets_test

import (
	"context"
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

// A taken name is the caller's own lost create: Create reports it with an empty UID, and neither
// reads nor replaces the Secret that holds it.
func TestCreateOfATakenNameLeavesTheSecret(t *testing.T) {
	cl := secretstest.New()
	s := k8ssecrets.New(cl, ns)
	ctx := context.Background()
	first, err := s.Create(ctx, "n", "mcp", "ml_one")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Create(ctx, "n", "mcp", "ml_one")
	if err != nil || again != "" {
		t.Errorf("taken name: uid %q, %v; want empty, nil", again, err)
	}
	if cl.UID(ns, "n") != first {
		t.Error("the Secret was replaced")
	}
	for _, a := range cl.Actions() {
		if a.GetVerb() == "get" {
			t.Error("Create read the existing Secret")
		}
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
	if _, err := s.Delete(ctx, "n", old); err != nil {
		t.Fatalf("Delete with a stale UID = %v, want nil", err)
	}
	if cl.UID(ns, "n") != fresh {
		t.Fatal("a delete aimed at the old UID removed the new Secret")
	}
	if found, err := s.Delete(ctx, "n", fresh); err != nil || !found {
		t.Fatal(found, err)
	}
	if cl.UID(ns, "n") != "" {
		t.Error("Secret not deleted")
	}
	// Already gone is success.
	if found, err := s.Delete(ctx, "n", fresh); err != nil || found {
		t.Errorf("Delete of a missing Secret = %v, %v", found, err)
	}
}

// Without a UID the delete goes by name, with no precondition, and a missing Secret is success.
func TestDeleteWithoutAUIDGoesByName(t *testing.T) {
	cl := secretstest.New()
	s := k8ssecrets.New(cl, ns)
	ctx := context.Background()
	if _, err := s.Create(ctx, "n", "mcp", "ml_one"); err != nil {
		t.Fatal(err)
	}
	if found, err := s.Delete(ctx, "n", ""); err != nil || !found {
		t.Fatal(found, err)
	}
	if cl.UID(ns, "n") != "" {
		t.Error("Secret not deleted")
	}
	if found, err := s.Delete(ctx, "n", ""); err != nil || found {
		t.Errorf("Delete of a missing Secret = %v, %v", found, err)
	}
	if len(cl.Deletes) != 2 || cl.Deletes[0] != "n@" {
		t.Errorf("deletes = %v, want two without a precondition", cl.Deletes)
	}
}

// The role grants create and delete only; any other verb, get included, would be refused in the
// cluster.
func TestOnlyCreateDelete(t *testing.T) {
	cl := secretstest.New()
	s := k8ssecrets.New(cl, ns)
	ctx := context.Background()
	uid, _ := s.Create(ctx, "n", "mcp", "ml_one")
	_, _ = s.Create(ctx, "n", "mcp", "ml_one")
	_, _ = s.Delete(ctx, "n", uid)
	for _, a := range cl.Actions() {
		switch a.GetVerb() {
		case "create", "delete":
		default:
			t.Errorf("verb %q used", a.GetVerb())
		}
	}
}
