// Package k8ssecrets keeps the bearer Secrets that carry agent credentials into the kagent
// namespace. A Secret is immutable: it is created once and deleted, never updated, so a token
// cannot be swapped under a running session. The package uses only create and delete, which is all
// the RBAC role grants: Mainloop never reads a Secret back, so it cannot read anything else in the
// namespace either (RBAC cannot limit get to a name prefix).
package k8ssecrets

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Labels and keys of a credential Secret. The egress label is what the gateway's secret selector
// matches; the data key is the one the gateway reads.
const (
	LabelActorEgress = "mainloop.dev/actor-egress"
	LabelPurpose     = "mainloop.dev/purpose"
	DataKey          = "authorization"
)

// Store creates and deletes credential Secrets in one namespace.
type Store struct {
	client    kubernetes.Interface
	namespace string
}

// New builds a Store over a client. Tests pass a fake clientset.
func New(client kubernetes.Interface, namespace string) *Store {
	return &Store{client: client, namespace: namespace}
}

// NewInCluster builds a Store from the pod's service account.
func NewInCluster(namespace string) (*Store, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("k8ssecrets: in-cluster config: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8ssecrets: build client: %w", err)
	}
	return New(client, namespace), nil
}

// Namespace is the namespace the Store works in.
func (s *Store) Namespace() string { return s.namespace }

func labels(purpose string) map[string]string {
	return map[string]string{LabelActorEgress: "true", LabelPurpose: purpose}
}

func authorization(bearer string) []byte { return []byte("Bearer " + bearer) }

// Create publishes the Secret and returns its UID. When the name is taken it returns an empty UID
// and no error: the caller's own earlier create landed and its response was lost. That holds
// because a name has one writer at a time (agentauth gives a name to a new credential only after
// the previous holder's Secret is deleted), and the existing Secret is never read or overwritten.
func (s *Store) Create(ctx context.Context, name, purpose, bearer string) (string, error) {
	immutable := true
	want := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace, Labels: labels(purpose)},
		Type:       corev1.SecretTypeOpaque,
		Immutable:  &immutable,
		Data:       map[string][]byte{DataKey: authorization(bearer)},
	}
	secrets := s.client.CoreV1().Secrets(s.namespace)
	created, err := secrets.Create(ctx, want, metav1.CreateOptions{})
	if err == nil {
		return string(created.UID), nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("k8ssecrets: create %s: %w", name, err)
	}
	return "", nil
}

// Delete removes the Secret with this name. With a UID, the precondition makes the delete apply only
// to the Secret that was published, and a Secret now under the name with a different UID is left
// alone. An empty UID (a create whose UID was never seen) deletes by name: the caller vouches that
// nothing else can hold the name. A Secret that is already gone, or has a different UID, counts as
// deleted; found reports whether this call removed one.
func (s *Store) Delete(ctx context.Context, name, uid string) (found bool, err error) {
	var opts metav1.DeleteOptions
	if uid != "" {
		u := types.UID(uid)
		opts.Preconditions = &metav1.Preconditions{UID: &u}
	}
	err = s.client.CoreV1().Secrets(s.namespace).Delete(ctx, name, opts)
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return false, nil
	}
	return false, fmt.Errorf("k8ssecrets: delete %s: %w", name, err)
}
