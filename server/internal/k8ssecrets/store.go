// Package k8ssecrets keeps the bearer Secrets that carry agent credentials into the kagent
// namespace. A Secret is immutable: it is created once and deleted, never updated, so a token
// cannot be swapped under a running session. The package uses only get, create and delete, which
// is all the RBAC role grants.
package k8ssecrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"

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

// ErrMismatch means a Secret already holds the name but is not the one asked for (different
// bearer, labels or mutability). The name is never overwritten.
var ErrMismatch = errors.New("k8ssecrets: existing Secret differs from the requested one")

// Store reads, creates and deletes credential Secrets in one namespace.
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

// Create publishes the Secret and returns its UID. When the name is taken, it reads the Secret
// and succeeds only if it is the same one (same labels, immutable, same bearer): a retry after a
// lost response then recovers the UID instead of failing. A different Secret is [ErrMismatch].
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
	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("k8ssecrets: read existing %s: %w", name, err)
	}
	if !same(existing, want) {
		return "", fmt.Errorf("%w: %s", ErrMismatch, name)
	}
	return string(existing.UID), nil
}

func same(have, want *corev1.Secret) bool {
	return have.Immutable != nil && *have.Immutable &&
		maps.Equal(have.Labels, want.Labels) &&
		len(have.Data) == 1 && bytes.Equal(have.Data[DataKey], want.Data[DataKey])
}

// Lookup returns the UID of the Secret with this name, or false when there is none.
func (s *Store) Lookup(ctx context.Context, name string) (uid string, found bool, err error) {
	sec, err := s.client.CoreV1().Secrets(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("k8ssecrets: read %s: %w", name, err)
	}
	return string(sec.UID), true, nil
}

// Delete removes the Secret with this name and UID. The UID precondition makes the delete apply
// only to the Secret that was published: a Secret created later under the same name (a new
// credential for the binding) is left alone. A Secret that is already gone, or that now has a
// different UID, counts as deleted.
func (s *Store) Delete(ctx context.Context, name, uid string) error {
	if uid == "" {
		return errors.New("k8ssecrets: delete needs the Secret's UID")
	}
	u := types.UID(uid)
	err := s.client.CoreV1().Secrets(s.namespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &u},
	})
	if err == nil || apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	return fmt.Errorf("k8ssecrets: delete %s: %w", name, err)
}
