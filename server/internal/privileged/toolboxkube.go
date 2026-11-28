package privileged

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// AccountToolbox is the executor's ServiceAccount for command.exec. Nothing runs as it: the
// executor mints a token for it and makes pods, and reads and deletes them, in the toolbox
// namespace only (the chart's Role).
const AccountToolbox = "mainloop-executor-toolbox"

// DefaultToolboxNamespace is where command.exec's pods run: a namespace of their own, with Pod
// Security restricted and a default-deny network policy (the chart).
const DefaultToolboxNamespace = "mainloop-toolbox"

// Labels on a run's pod and Secret.
const (
	labelToolbox  = "app.kubernetes.io/name"
	labelToolboxV = "mainloop-toolbox"
	labelAction   = "mainloop.dev/privileged-action"
	labelReach    = "mainloop.dev/reach"
)

const (
	toolboxContainer = "run"
	proxyContainer   = "kube-proxy"
	// kubeSecretSuffix names a cluster-reach run's credential Secret, beside the script's.
	kubeSecretSuffix = "-kube"
	// secretsSecretSuffix names a secrets run's values, mounted at /run/secrets.
	secretsSecretSuffix = "-secrets"
	// secretsDir is where the script's container finds each secret, as a file named after it.
	secretsDir = "/run/secrets"
	toolboxUID = 65532
	// scriptDir holds the run's script and stdin, from the run's Secret.
	scriptDir = "/run/exec"
	// execKubeCreds is where the proxy's container mounts the run's credential, and execKubeDir the
	// volume the proxy writes the kubeconfig to and the script's container reads it from.
	execKubeCreds = "/run/exec-kube" //nolint:gosec // a mount path, not a credential
	execKubeDir   = "/run/kube"
	// toolboxGrace is what a pod's deadline allows beyond the script's timeout: the image's pull
	// counts against it, and so do the 5 seconds between the script's TERM and KILL.
	toolboxGrace = toolboxPendingLimit + time.Minute
)

// KubeToolbox runs the toolbox pods through the Kubernetes API with the executor's toolbox
// account. Clients mints that account's token, refreshed before it expires; Client, if set,
// replaces it (tests).
type KubeToolbox struct {
	Clients   KubeClients
	Namespace string
	// PullSecrets name Secrets in Namespace that the image's registry needs.
	PullSecrets []string
	Client      kubernetes.Interface
	// Policies is set when the toolbox takes secrets: each such run has a CiliumNetworkPolicy the
	// toolbox account makes and deletes.
	Policies bool
	// Dynamic makes a secrets run's CiliumNetworkPolicy; replaces minting it (tests).
	Dynamic dynamic.Interface
	// AdminCredential, if set, replaces minting the cluster-admin credential of a cluster-reach run
	// through Clients (tests).
	AdminCredential func(ctx context.Context, lifetime time.Duration) (KubeCredential, error)

	mu        sync.Mutex
	minted    kubernetes.Interface
	mintedDyn dynamic.Interface
	expires   time.Time
}

const toolboxTokenLife = 20 * time.Minute

// client returns a client for the toolbox account, minted again when under five minutes remain.
func (k *KubeToolbox) client(ctx context.Context) (kubernetes.Interface, error) {
	if k.Client != nil {
		return k.Client, nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.minted != nil && time.Until(k.expires) > 5*time.Minute {
		return k.minted, nil
	}
	cfg, err := k.Clients.ConfigFor(ctx, AccountToolbox, toolboxTokenLife)
	if err != nil {
		return nil, err
	}
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("toolbox client: %w", err)
	}
	d, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("toolbox client: %w", err)
	}
	k.minted, k.mintedDyn, k.expires = c, d, time.Now().Add(toolboxTokenLife)
	return c, nil
}

// dyn returns the dynamic client for the toolbox account (made with client's token).
func (k *KubeToolbox) dyn(ctx context.Context) (dynamic.Interface, error) {
	if k.Dynamic != nil {
		return k.Dynamic, nil
	}
	if _, err := k.client(ctx); err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.mintedDyn, nil
}

var policyGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}

// hostsPolicy is a secrets run's network policy, selecting its pod alone: DNS for the run's hosts,
// and TCP 443 to them. The pod carries the pinned reach label, which no deployment policy opens.
func hostsPolicy(name, ns, actionID string, labels map[string]string, hosts []string) *unstructured.Unstructured {
	dnsRules, fqdns := []any{}, []any{}
	for _, h := range hosts {
		dnsRules = append(dnsRules, map[string]any{"matchName": h})
		fqdns = append(fqdns, map[string]any{"matchName": h})
	}
	ls := map[string]any{}
	for k, v := range labels {
		ls[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": ls},
		"spec": map[string]any{
			"endpointSelector": map[string]any{"matchLabels": map[string]any{labelAction: actionID}},
			"ingress":          []any{},
			"egress": []any{
				map[string]any{
					"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s-app": "kube-dns"}}},
					"toPorts": []any{map[string]any{
						"ports": []any{map[string]any{"port": "53", "protocol": "UDP"}, map[string]any{"port": "53", "protocol": "TCP"}},
						"rules": map[string]any{"dns": dnsRules},
					}},
				},
				map[string]any{
					"toFQDNs": fqdns,
					"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "443", "protocol": "TCP"}}}},
				},
			},
		},
	}}
}

// KubeCredential is what a cluster-reach run's proxy needs: a cluster-admin token minted for the
// run, the API server's address and its certificate authority.
type KubeCredential struct {
	Token, Host, CA string
}

// adminCredential mints the run's cluster-admin token, valid for the lifetime, with the executor's
// own API server address and certificate authority.
func (k *KubeToolbox) adminCredential(ctx context.Context, lifetime time.Duration) (KubeCredential, error) {
	if k.AdminCredential != nil {
		return k.AdminCredential(ctx, lifetime)
	}
	cfg, err := k.Clients.ConfigFor(ctx, AccountAdmin, lifetime)
	if err != nil {
		return KubeCredential{}, err
	}
	ca := cfg.CAData
	if len(ca) == 0 && cfg.CAFile != "" {
		if ca, err = os.ReadFile(cfg.CAFile); err != nil {
			return KubeCredential{}, fmt.Errorf("the cluster's certificate authority: %w", err)
		}
	}
	return KubeCredential{Token: cfg.BearerToken, Host: cfg.Host, CA: string(ca)}, nil
}

func (k *KubeToolbox) ns() string {
	if k.Namespace == "" {
		return DefaultToolboxNamespace
	}
	return k.Namespace
}

// Ready lists pods, which needs the account, the namespace and its Role.
func (k *KubeToolbox) Ready(ctx context.Context) error {
	c, err := k.client(ctx)
	if err != nil {
		return err
	}
	_, err = c.CoreV1().Pods(k.ns()).List(ctx, metav1.ListOptions{Limit: 1, LabelSelector: labelToolbox + "=" + labelToolboxV})
	switch {
	case apierrors.IsForbidden(err), apierrors.IsNotFound(err), apierrors.IsUnauthorized(err):
		return refuse("the toolbox is not ready: its namespace %s or the executor's access to it is missing", k.ns())
	}
	return err
}

func runLabels(actionID, reach string) map[string]string {
	return map[string]string{labelToolbox: labelToolboxV, labelAction: actionID, labelReach: reach}
}

// Create makes the run's Secret and pod. Both are named after the action, so creating them again
// finds the same run.
func (k *KubeToolbox) Create(ctx context.Context, spec ToolboxSpec) error {
	c, err := k.client(ctx)
	if err != nil {
		return err
	}
	name := execPodName(spec.ActionID)
	reach := spec.Reach
	if len(spec.Secrets) > 0 {
		reach = reachPinned
	}
	labels := runLabels(spec.ActionID, reach)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.ns(), Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"script": []byte(spec.Script), "stdin": []byte(spec.Stdin)},
	}
	if _, err := c.CoreV1().Secrets(k.ns()).Create(ctx, secret, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
		return created(err)
	}
	cleanup := func() {
		// No pod will read them.
		_ = c.CoreV1().Secrets(k.ns()).Delete(ctx, name, metav1.DeleteOptions{})
		_ = c.CoreV1().Secrets(k.ns()).Delete(ctx, name+kubeSecretSuffix, metav1.DeleteOptions{})
		_ = c.CoreV1().Secrets(k.ns()).Delete(ctx, name+secretsSecretSuffix, metav1.DeleteOptions{})
		if d, err := k.dyn(ctx); err == nil && len(spec.Secrets) > 0 {
			_ = d.Resource(policyGVR).Namespace(k.ns()).Delete(ctx, name, metav1.DeleteOptions{})
		}
	}
	if len(spec.Secrets) > 0 {
		// The values and the only hosts they may go to, before the pod that reads them: the policy
		// first, so the pod never starts with more network than it was approved.
		d, err := k.dyn(ctx)
		if err != nil {
			cleanup()
			return err
		}
		if _, err := d.Resource(policyGVR).Namespace(k.ns()).Create(ctx, hostsPolicy(name, k.ns(), spec.ActionID, labels, spec.Hosts), metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
			cleanup()
			return created(err)
		}
		data := map[string][]byte{}
		for n, v := range spec.Secrets {
			data[n] = slices.Clone(v)
		}
		vals := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + secretsSecretSuffix, Namespace: k.ns(), Labels: labels},
			Type:       corev1.SecretTypeOpaque, Data: data,
		}
		if _, err := c.CoreV1().Secrets(k.ns()).Create(ctx, vals, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
			cleanup()
			return created(err)
		}
	}
	if spec.Reach == ReachCluster {
		// The token outlives the pod's own deadline, so it cannot expire under the script.
		cred, err := k.adminCredential(ctx, spec.Timeout+toolboxGrace+time.Minute)
		if err != nil {
			cleanup()
			return err // nothing was made that ran: the next attempt starts again
		}
		kube := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + kubeSecretSuffix, Namespace: k.ns(), Labels: labels},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{execKubeToken: []byte(cred.Token), execKubeHost: []byte(cred.Host), execKubeCA: []byte(cred.CA)},
		}
		if _, err := c.CoreV1().Secrets(k.ns()).Create(ctx, kube, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
			cleanup()
			return created(err)
		}
	}
	if _, err := c.CoreV1().Pods(k.ns()).Create(ctx, k.pod(name, labels, spec), metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
		err = created(err)
		var before *NothingRan
		if errors.As(err, &before) {
			cleanup()
		}
		return err
	}
	return nil
}

// created classifies a create's error: an answer from the API server that refuses the object means
// nothing was made; any other error (a lost connection) may have left it made.
func created(err error) error {
	var s apierrors.APIStatus
	if errors.As(err, &s) && s.Status().Code >= 400 && s.Status().Code < 500 && s.Status().Code != 408 && s.Status().Code != 429 {
		return &NothingRan{Err: fmt.Errorf("the API server refused the toolbox pod (%s)", apierrors.ReasonForError(err))}
	}
	return err
}

// pod is the run's pod: restricted, with no credential, a read-only root, and the script mounted
// from the run's Secret. Its own deadline outlasts the script's timeout by the image's pull and
// the kill grace.
func (k *KubeToolbox) pod(name string, labels map[string]string, spec ToolboxSpec) *corev1.Pod {
	f, t := false, true
	deadline := int64((spec.Timeout + toolboxGrace).Seconds())
	uid := int64(toolboxUID)
	secs := strconv.Itoa(int(spec.Timeout.Seconds()))
	var pull []corev1.LocalObjectReference
	for _, s := range k.PullSecrets {
		pull = append(pull, corev1.LocalObjectReference{Name: s})
	}
	script := `exec timeout -k 5 "$1" bash -euo pipefail ` + scriptDir + `/script <` + scriptDir + `/stdin`
	env := []corev1.EnvVar{{Name: "HOME", Value: "/work"}}
	mounts := []corev1.VolumeMount{
		{Name: "work", MountPath: "/work"},
		{Name: "tmp", MountPath: "/tmp"},
		{Name: "exec", MountPath: scriptDir, ReadOnly: true},
	}
	volumes := []corev1.Volume{
		{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(1<<30, resource.BinarySI)}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(256<<20, resource.BinarySI)}}},
		{Name: "exec", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name, DefaultMode: new(int32(0o444))}}},
	}
	if len(spec.Secrets) > 0 {
		mounts = append(mounts, corev1.VolumeMount{Name: "run-secrets", MountPath: secretsDir, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "run-secrets", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: name + secretsSecretSuffix, DefaultMode: new(int32(0o444))}}})
	}
	var sidecars []corev1.Container
	if spec.Reach == ReachCluster {
		// The proxy is a native sidecar: it starts first, is ready once its kubeconfig exists, and
		// is stopped when the script's container ends. Only it mounts the run's credential; the
		// script's container gets the kubeconfig, which names the proxy and a token worth nothing
		// elsewhere.
		kubeconfig := execKubeDir + "/" + ExecKubeConfigFile
		script = `for i in $(seq 1 120); do [ -s ` + kubeconfig + ` ] && break; sleep 0.5; done
if [ ! -s ` + kubeconfig + ` ]; then printf %s '` + proxyNotReady + `' >/dev/termination-log; exit 97; fi
export KUBECONFIG=` + kubeconfig + `
` + script
		env = append(env, corev1.EnvVar{Name: "KUBECONFIG", Value: kubeconfig})
		mounts = append(mounts, corev1.VolumeMount{Name: "kube", MountPath: execKubeDir, ReadOnly: true})
		volumes = append(volumes,
			corev1.Volume{Name: "kube", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: resource.NewQuantity(1<<20, resource.BinarySI)}}},
			corev1.Volume{Name: "kube-credential", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name + kubeSecretSuffix, DefaultMode: new(int32(0o444))}}})
		always := corev1.ContainerRestartPolicyAlways
		sidecars = []corev1.Container{{
			Name: proxyContainer, Image: spec.ProxyImage, ImagePullPolicy: corev1.PullIfNotPresent, RestartPolicy: &always,
			Command: []string{"/usr/local/bin/mainloop", "exec-kube-proxy"},
			Env: []corev1.EnvVar{
				{Name: "MAINLOOP_EXEC_KUBE_CREDS", Value: execKubeCreds},
				{Name: "MAINLOOP_EXEC_KUBE_OUT", Value: execKubeDir},
				{Name: "MAINLOOP_EXEC_KUBE_PROTECT", Value: k.Clients.Namespace + "," + k.ns()},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &f, ReadOnlyRootFilesystem: &t, RunAsNonRoot: &t, RunAsUser: &uid, RunAsGroup: &uid,
				Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("64Mi")},
			},
			StartupProbe: &corev1.Probe{
				ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"test", "-s", kubeconfig}}},
				PeriodSeconds:    1,
				FailureThreshold: 60,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "kube-credential", MountPath: execKubeCreds, ReadOnly: true},
				{Name: "kube", MountPath: execKubeDir},
			},
		}}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.ns(), Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: &deadline,
			// No ServiceAccount token, no service links: the run holds no Kubernetes credential.
			AutomountServiceAccountToken: &f,
			EnableServiceLinks:           &f,
			ImagePullSecrets:             pull,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &t, RunAsUser: &uid, RunAsGroup: &uid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				// ping works without NET_RAW through unprivileged ICMP sockets.
				Sysctls: []corev1.Sysctl{{Name: "net.ipv4.ping_group_range", Value: "0 2147483647"}},
			},
			InitContainers: sidecars,
			Containers: []corev1.Container{{
				Name: toolboxContainer, Image: spec.Image, ImagePullPolicy: corev1.PullIfNotPresent,
				// The deadline is the container's own: timeout stops the script, and its exit 124
				// tells the executor it ran out. The script is a file, never a command line.
				Command: []string{"bash", "-c", script, "toolbox", secs},
				Env:     env,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &f, ReadOnlyRootFilesystem: &t, RunAsNonRoot: &t, RunAsUser: &uid, RunAsGroup: &uid,
					Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("2Gi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
					},
				},
				VolumeMounts: mounts,
			}},
			Volumes: volumes,
		},
	}
}

// State reads the run's pod and maps it: a container that has not started means nothing ran.
func (k *KubeToolbox) State(ctx context.Context, actionID string) (ToolboxState, error) {
	c, err := k.client(ctx)
	if err != nil {
		return ToolboxState{}, err
	}
	p, err := c.CoreV1().Pods(k.ns()).Get(ctx, execPodName(actionID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ToolboxState{}, nil
	}
	if err != nil {
		return ToolboxState{}, err
	}
	return podState(p), nil
}

func podState(p *corev1.Pod) ToolboxState {
	st := ToolboxState{Exists: true, Phase: ToolboxPending, Reason: string(p.Status.Phase)}
	var cs *corev1.ContainerStatus
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == toolboxContainer {
			cs = &p.Status.ContainerStatuses[i]
		}
	}
	switch {
	case cs != nil && cs.State.Terminated != nil:
		t := cs.State.Terminated
		// A container that terminated without ever starting could not be created: nothing ran.
		st.Started = !t.StartedAt.IsZero()
		st.Phase, st.ExitCode, st.Reason, st.Message = ToolboxExited, int(t.ExitCode), t.Reason, t.Message
		if st.Reason == "" {
			st.Reason = "Completed"
		}
		if !st.Started {
			st.Phase = ToolboxFailed
		}
		if p.Status.Reason == "DeadlineExceeded" {
			st.Reason = p.Status.Reason // the pod's deadline killed it
		}
	case p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded:
		st.Phase = ToolboxFailed
		st.Started = cs != nil && cs.Started != nil && *cs.Started
		if p.Status.Reason != "" {
			st.Reason = p.Status.Reason
		}
	case cs != nil && cs.State.Running != nil:
		st.Phase, st.Started = ToolboxRunning, true
	case cs != nil && cs.State.Waiting != nil:
		st.Reason, st.Message = cs.State.Waiting.Reason, cs.State.Waiting.Message
	}
	return st
}

// Output reads the pod's log: stdout and stderr together, at most max bytes.
func (k *KubeToolbox) Output(ctx context.Context, actionID string, max int) ([]byte, error) {
	c, err := k.client(ctx)
	if err != nil {
		return nil, err
	}
	lim := int64(max)
	rc, err := c.CoreV1().Pods(k.ns()).GetLogs(execPodName(actionID), &corev1.PodLogOptions{Container: toolboxContainer, LimitBytes: &lim}).Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, lim))
}

// RunSecrets reads the values the run holds from its own Secret.
func (k *KubeToolbox) RunSecrets(ctx context.Context, actionID string) (map[string][]byte, error) {
	c, err := k.client(ctx)
	if err != nil {
		return nil, err
	}
	s, err := c.CoreV1().Secrets(k.ns()).Get(ctx, execPodName(actionID)+secretsSecretSuffix, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return s.Data, nil
}

// Delete removes the run's pod and Secret.
func (k *KubeToolbox) Delete(ctx context.Context, actionID string) error {
	c, err := k.client(ctx)
	if err != nil {
		return err
	}
	name := execPodName(actionID)
	grace := int64(0)
	if err := c.CoreV1().Pods(k.ns()).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &grace}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	for _, secret := range []string{name, name + kubeSecretSuffix, name + secretsSecretSuffix} {
		if err := c.CoreV1().Secrets(k.ns()).Delete(ctx, secret, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if k.Policies {
		d, err := k.dyn(ctx)
		if err != nil {
			return err
		}
		if err := d.Resource(policyGVR).Namespace(k.ns()).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return err
		}
	}
	return nil
}

// Runs lists the runs that have a pod or a Secret.
func (k *KubeToolbox) Runs(ctx context.Context) ([]ToolboxRef, error) {
	c, err := k.client(ctx)
	if err != nil {
		return nil, err
	}
	opts := metav1.ListOptions{LabelSelector: labelToolbox + "=" + labelToolboxV}
	pods, err := c.CoreV1().Pods(k.ns()).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	secrets, err := c.CoreV1().Secrets(k.ns()).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	seen := map[string]int{}
	var out []ToolboxRef
	add := func(l map[string]string, at time.Time) {
		id := l[labelAction]
		if id == "" {
			return
		}
		if i, ok := seen[id]; ok {
			out[i].CreatedAt = minTime(out[i].CreatedAt, at)
			return
		}
		seen[id] = len(out)
		out = append(out, ToolboxRef{ActionID: id, CreatedAt: at})
	}
	for _, p := range pods.Items {
		add(p.Labels, p.CreationTimestamp.Time)
	}
	for _, s := range secrets.Items {
		add(s.Labels, s.CreationTimestamp.Time)
	}
	return out, nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
