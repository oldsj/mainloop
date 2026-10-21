package devloop

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// PodsFor resolves the [PodClient] and the Kubernetes namespace to use for t: the in-cluster
// client directly, scoped to t.Namespace, for [TargetNamespace]; a client built from the
// vcluster's own kubeconfig, scoped to t.VClusterNamespace, for [TargetVCluster]. Only serve-api
// calls this (the only process with Pods, and optionally VClusters, configured); serve-mcp never
// reaches Kubernetes at all, per the package doc.
func (s *OperationsService) PodsFor(ctx context.Context, t Target) (PodClient, string, error) {
	switch t.Kind {
	case TargetVCluster:
		if s.VClusters == nil {
			return nil, "", ErrVClusterNotConfigured
		}
		return s.VClusters.ForVCluster(ctx, t)
	default:
		if s.Pods == nil {
			return nil, "", errors.New("devloop: kubernetes is not configured")
		}
		return s.Pods, t.Namespace, nil
	}
}

// VClusterClientFactory builds the [PodClient] and inner namespace for a [TargetVCluster] target.
// Production leaves [OperationsService.VClusters] nil until the infra grant described in
// docs/specs/devloop.md exists (a Role to read the vcluster's kubeconfig Secret, bound to
// serve-api's ServiceAccount): every vcluster sync then fails clearly with
// [ErrVClusterNotConfigured] instead of reaching for a credential that is not there.
type VClusterClientFactory interface {
	ForVCluster(ctx context.Context, t Target) (PodClient, string, error)
}

// ErrVClusterNotConfigured is what every vcluster-kind target fails with until a
// [VClusterClientFactory] is wired in production.
var ErrVClusterNotConfigured = errors.New("devloop: vcluster targets are not configured (no vcluster client wired)")

// SecretReader reads one key of one Kubernetes Secret: the minimal surface a vcluster target
// needs, its own kubeconfig, stored by convention in a Secret in its host namespace. [K8sSecrets]
// is the real implementation; tests use a fake.
type SecretReader interface {
	ReadSecretKey(ctx context.Context, namespace, name, key string) ([]byte, error)
}

// K8sSecrets is the real [SecretReader], backed by client-go. Not wired into production by this
// slice (see docs/specs/devloop.md's vcluster section for the RBAC grant it needs first); building
// one does not itself grant any access.
type K8sSecrets struct {
	Clientset kubernetes.Interface
}

func (k *K8sSecrets) ReadSecretKey(ctx context.Context, namespace, name, key string) ([]byte, error) {
	secret, err := k.Clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	v, ok := secret.Data[key]
	if !ok {
		return nil, fmt.Errorf("devloop: secret %s/%s has no key %q", namespace, name, key)
	}
	return v, nil
}

var _ SecretReader = (*K8sSecrets)(nil)

// VClusterPods is a [VClusterClientFactory] that reads the vcluster's kubeconfig from its host
// namespace (the "vc-<name>" Secret vcluster's own chart writes there, key "config") through
// Secrets, and builds a client-go client from it. Reachability, not policy: a SecretReader that
// can in fact read the Secret still needs the vcluster's own RBAC inside itself to list pods, log
// and exec in VClusterNamespace, which is a separate grant inside the vcluster (see
// docs/specs/devloop.md).
type VClusterPods struct {
	Secrets SecretReader
}

func (v *VClusterPods) ForVCluster(ctx context.Context, t Target) (PodClient, string, error) {
	if v == nil || v.Secrets == nil {
		return nil, "", ErrVClusterNotConfigured
	}
	kubeconfig, err := v.Secrets.ReadSecretKey(ctx, t.Namespace, "vc-"+t.VClusterName, "config")
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrVClusterNotConfigured, err)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, "", fmt.Errorf("devloop: vcluster %s kubeconfig: %w", t.VClusterName, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("devloop: vcluster %s client: %w", t.VClusterName, err)
	}
	return NewPods(cs, cfg), t.VClusterNamespace, nil
}

var _ VClusterClientFactory = (*VClusterPods)(nil)
