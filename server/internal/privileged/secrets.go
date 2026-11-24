package privileged

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Kubernetes names the executor writes with.
const (
	// AnnotationAction names the privileged action that last wrote the Secret.
	AnnotationAction = "mainloop.dev/privileged-action"
	// FieldManager is the executor's field manager on every write.
	FieldManager = "mainloop-executor"
)

// SecretMeta is what preflight and the observe step read about a target Secret: metadata and key
// names only, never data.
type SecretMeta struct {
	Exists          bool     `json:"exists"`
	UID             string   `json:"uid,omitempty"`
	ResourceVersion string   `json:"resource_version,omitempty"`
	Type            string   `json:"type,omitempty"`
	Keys            []string `json:"keys,omitempty"`
	// Controller is the controlling owner (kind/name), which would undo a change.
	Controller  string `json:"controller,omitempty"`
	ArgoTracked bool   `json:"argo_tracked,omitempty"`
	ManagedBy   string `json:"managed_by,omitempty"`
	// Action is the privileged action that last wrote it, if any.
	Action string `json:"action,omitempty"`
	// Immutable Secrets cannot be patched.
	Immutable bool `json:"immutable,omitempty"`
}

// Secrets reads and writes the target Secrets of onepassword.item_to_secret.
type Secrets interface {
	Inspect(ctx context.Context, namespace, name string) (SecretMeta, error)
	// Create makes an Opaque Secret with one key. One that exists already is ErrTargetChanged.
	Create(ctx context.Context, namespace, name, key string, value []byte, actionID string) (string, error)
	// Patch sets one key of an existing Secret, only at resourceVersion: a Secret changed or deleted
	// since is ErrTargetChanged. Other keys are left as they are.
	Patch(ctx context.Context, namespace, name, key string, value []byte, actionID, resourceVersion string) (string, error)
}

// KubeSecrets is Secrets over the executor's Kubernetes client. A JSON merge patch carrying the
// resourceVersion sets one key and fails on any change since preflight. Server-side apply is not
// used: an apply that names only this key would remove keys an earlier action wrote under the
// same field manager.
type KubeSecrets struct {
	Client kubernetes.Interface
}

// Inspect reads the Secret and keeps only metadata and key names.
func (k KubeSecrets) Inspect(ctx context.Context, namespace, name string) (SecretMeta, error) {
	s, err := k.Client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return SecretMeta{}, nil
	}
	if err != nil {
		return SecretMeta{}, kubeError(err)
	}
	m := SecretMeta{
		Exists: true, UID: string(s.UID), ResourceVersion: s.ResourceVersion, Type: string(s.Type),
		ManagedBy: s.Labels["app.kubernetes.io/managed-by"], Action: s.Annotations[AnnotationAction],
		ArgoTracked: s.Annotations["argocd.argoproj.io/tracking-id"] != "" || s.Labels["argocd.argoproj.io/instance"] != "",
		Immutable:   s.Immutable != nil && *s.Immutable,
	}
	if m.Type == "" {
		m.Type = string(corev1.SecretTypeOpaque)
	}
	for key := range s.Data {
		m.Keys = append(m.Keys, key)
	}
	for key := range s.StringData {
		m.Keys = append(m.Keys, key)
	}
	sort.Strings(m.Keys)
	if c := metav1.GetControllerOf(s); c != nil {
		m.Controller = c.Kind + "/" + c.Name
	}
	return m, nil
}

// Create makes the Secret.
func (k KubeSecrets) Create(ctx context.Context, namespace, name, key string, value []byte, actionID string) (string, error) {
	s, err := k.Client.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: map[string]string{AnnotationAction: actionID}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{key: value},
	}, metav1.CreateOptions{FieldManager: FieldManager})
	if apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("%w: it was created meanwhile", ErrTargetChanged)
	}
	if err != nil {
		return "", kubeError(err)
	}
	return s.ResourceVersion, nil
}

// Patch sets the key at resourceVersion.
func (k KubeSecrets) Patch(ctx context.Context, namespace, name, key string, value []byte, actionID, resourceVersion string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"resourceVersion": resourceVersion,
			"annotations":     map[string]string{AnnotationAction: actionID},
		},
		"data": map[string][]byte{key: value},
	})
	if err != nil {
		return "", err
	}
	defer clear(body)
	s, err := k.Client.CoreV1().Secrets(namespace).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{FieldManager: FieldManager})
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return "", fmt.Errorf("%w: it changed or was deleted meanwhile", ErrTargetChanged)
	}
	if err != nil {
		return "", kubeError(err)
	}
	return s.ResourceVersion, nil
}

// kubeError keeps the status of an API error and drops its body.
func kubeError(err error) error {
	if apierrors.IsForbidden(err) {
		return fmt.Errorf("%w: Kubernetes refused (forbidden)", ErrForbidden)
	}
	if s, ok := err.(apierrors.APIStatus); ok {
		code := s.Status().Code
		if code >= 400 && code < 500 && !slices.Contains([]int32{401, 404, 408, 409, 429}, code) {
			return fmt.Errorf("%w: kubernetes answered %d %s", ErrRejected, code, s.Status().Reason)
		}
		return fmt.Errorf("kubernetes answered %d %s", code, s.Status().Reason)
	}
	return err
}
