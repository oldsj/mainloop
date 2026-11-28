package privileged_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/oldsj/mainloop/server/internal/privileged"
)

func newKubeToolbox() (*privileged.KubeToolbox, *fake.Clientset) {
	c := fake.NewClientset()
	return &privileged.KubeToolbox{Client: c, Namespace: "mainloop-toolbox", PullSecrets: []string{"ghcr-pull"}}, c
}

func toolboxSpec() privileged.ToolboxSpec {
	return privileged.ToolboxSpec{
		ActionID: "pa_0b6f1c2e-1111-4222-8333-444455556666", Image: toolboxImage, Reach: "internet",
		Script: "dig +short example.org\n", Stdin: "in\n", Timeout: 90 * time.Second,
	}
}

// A run's pod is restricted in every way the namespace's Pod Security level asks: non-root with a
// read-only root and no capabilities, no ServiceAccount token, no service links, a deadline that
// outlasts the script's timeout, the script in a Secret mounted read-only, and a name that is a
// valid DNS label made from the action's id.
func TestKubeToolboxPod(t *testing.T) {
	k, c := newKubeToolbox()
	spec := toolboxSpec()
	if err := k.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := k.Create(context.Background(), spec); err != nil {
		t.Fatalf("creating the same run again: %v", err)
	}
	const name = "exec-pa-0b6f1c2e-1111-4222-8333-444455556666"
	p, err := c.CoreV1().Pods("mainloop-toolbox").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := p.Spec
	cs := s.Containers[0]
	if len(s.Containers) != 1 || cs.Image != toolboxImage || cs.ImagePullPolicy != corev1.PullIfNotPresent ||
		s.RestartPolicy != corev1.RestartPolicyNever || *s.AutomountServiceAccountToken || *s.EnableServiceLinks ||
		s.HostNetwork || s.HostPID || s.HostIPC || s.ServiceAccountName != "" {
		t.Fatalf("pod = %+v", s)
	}
	if *s.ActiveDeadlineSeconds <= 90 || *s.ActiveDeadlineSeconds > 90+int64((10*time.Minute).Seconds()) {
		t.Errorf("deadline = %d", *s.ActiveDeadlineSeconds)
	}
	sc := cs.SecurityContext
	if *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || !*sc.RunAsNonRoot || *sc.RunAsUser != 65532 ||
		!slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(sc.Capabilities.Add) != 0 ||
		sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || sc.Privileged != nil {
		t.Errorf("container security = %+v", sc)
	}
	if !*s.SecurityContext.RunAsNonRoot || len(s.SecurityContext.Sysctls) != 1 || s.SecurityContext.Sysctls[0].Name != "net.ipv4.ping_group_range" {
		t.Errorf("pod security = %+v", s.SecurityContext)
	}
	if cs.Resources.Limits.Cpu().String() != "1" || cs.Resources.Limits.Memory().String() != "2Gi" {
		t.Errorf("limits = %+v", cs.Resources.Limits)
	}
	if len(s.ImagePullSecrets) != 1 || s.ImagePullSecrets[0].Name != "ghcr-pull" {
		t.Errorf("pull secrets = %+v", s.ImagePullSecrets)
	}
	// The command is fixed: the script is a file, never a word of the command line, and the timeout
	// is the container's own.
	cmd := strings.Join(cs.Command, " ")
	if strings.Contains(cmd, "dig") || !strings.Contains(cmd, `timeout -k 5 "$1" bash -euo pipefail /run/exec/script </run/exec/stdin`) || cs.Command[len(cs.Command)-1] != "90" {
		t.Errorf("command = %q", cmd)
	}
	for _, v := range s.Volumes {
		if v.HostPath != nil || v.PersistentVolumeClaim != nil {
			t.Errorf("volume %s is not ephemeral", v.Name)
		}
	}
	for _, m := range cs.VolumeMounts {
		if m.MountPath == "/run/exec" && !m.ReadOnly {
			t.Error("the script's mount is writable")
		}
	}
	if p.Labels["mainloop.dev/privileged-action"] != spec.ActionID || p.Labels["mainloop.dev/reach"] != "internet" {
		t.Errorf("labels = %v", p.Labels)
	}
	sec, err := c.CoreV1().Secrets("mainloop-toolbox").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil || string(sec.Data["script"]) != spec.Script || string(sec.Data["stdin"]) != spec.Stdin {
		t.Fatalf("secret = %+v (%v)", sec, err)
	}
}

// The API server's refusal means nothing ran, and leaves no Secret behind; a lost connection does
// not say so.
func TestKubeToolboxCreateErrors(t *testing.T) {
	k, c := newKubeToolbox()
	gr := schema.GroupResource{Resource: "pods"}
	c.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(gr, "x", errors.New("exceeded quota"))
	})
	err := k.Create(context.Background(), toolboxSpec())
	var before *privileged.NothingRan
	if !errors.As(err, &before) || strings.Contains(err.Error(), "quota") {
		t.Fatalf("err = %v", err)
	}
	if l, _ := c.CoreV1().Secrets("mainloop-toolbox").List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
		t.Errorf("a Secret is left: %d", len(l.Items))
	}

	k, c = newKubeToolbox()
	c.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection reset by peer")
	})
	if err := k.Create(context.Background(), toolboxSpec()); err == nil || errors.As(err, &before) {
		t.Fatalf("a lost connection said nothing ran: %v", err)
	}
}

func podWith(name string, phase corev1.PodPhase, status corev1.ContainerStatus, reason string) *corev1.Pod {
	status.Name = "run"
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "mainloop-toolbox", Labels: map[string]string{
			"app.kubernetes.io/name": "mainloop-toolbox", "mainloop.dev/privileged-action": "pa_x",
		}},
		Status: corev1.PodStatus{Phase: phase, Reason: reason, ContainerStatuses: []corev1.ContainerStatus{status}},
	}
}

// The pod's state is read from its container: waiting means nothing ran; running and terminated
// mean the script started.
func TestKubeToolboxState(t *testing.T) {
	started := metav1.Now()
	yes := true
	for name, tc := range map[string]struct {
		pod  *corev1.Pod
		want privileged.ToolboxState
	}{
		"pulling": {podWith("exec-pa-x", corev1.PodPending, corev1.ContainerStatus{State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}, ""),
			privileged.ToolboxState{Exists: true, Phase: privileged.ToolboxPending, Reason: "ImagePullBackOff"}},
		"running": {podWith("exec-pa-x", corev1.PodRunning, corev1.ContainerStatus{State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: started}}}, ""),
			privileged.ToolboxState{Exists: true, Phase: privileged.ToolboxRunning, Started: true, Reason: "Running"}},
		"exited": {podWith("exec-pa-x", corev1.PodSucceeded, corev1.ContainerStatus{State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, StartedAt: started, Reason: "Completed"}}}, ""),
			privileged.ToolboxState{Exists: true, Phase: privileged.ToolboxExited, Started: true, Reason: "Completed"}},
		"exit 3": {podWith("exec-pa-x", corev1.PodFailed, corev1.ContainerStatus{State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 3, StartedAt: started, Reason: "Error"}}}, ""),
			privileged.ToolboxState{Exists: true, Phase: privileged.ToolboxExited, Started: true, ExitCode: 3, Reason: "Error"}},
		"deadline": {podWith("exec-pa-x", corev1.PodFailed, corev1.ContainerStatus{State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, StartedAt: started, Reason: "Error"}}}, "DeadlineExceeded"),
			privileged.ToolboxState{Exists: true, Phase: privileged.ToolboxExited, Started: true, ExitCode: 137, Reason: "DeadlineExceeded"}},
		"could not be created": {podWith("exec-pa-x", corev1.PodFailed, corev1.ContainerStatus{State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 128, Reason: "ContainerCannotRun"}}}, ""),
			privileged.ToolboxState{Exists: true, Phase: privileged.ToolboxFailed, ExitCode: 128, Reason: "ContainerCannotRun"}},
		"evicted": {podWith("exec-pa-x", corev1.PodFailed, corev1.ContainerStatus{Started: &yes}, "Evicted"),
			privileged.ToolboxState{Exists: true, Phase: privileged.ToolboxFailed, Started: true, Reason: "Evicted"}},
	} {
		t.Run(name, func(t *testing.T) {
			k, c := newKubeToolbox()
			tc.pod.Name = "exec-pa-x"
			if _, err := c.CoreV1().Pods("mainloop-toolbox").Create(context.Background(), tc.pod, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			got, err := k.State(context.Background(), "pa_x")
			if err != nil || got != tc.want {
				t.Fatalf("state = %+v (%v), want %+v", got, err, tc.want)
			}
		})
	}
	k, _ := newKubeToolbox()
	if st, err := k.State(context.Background(), "pa_missing"); err != nil || st.Exists {
		t.Fatalf("a missing pod: %+v (%v)", st, err)
	}
}

// Ready needs the namespace and the account's Role; Runs, Output and Delete work on the run's
// pod and Secret.
func TestKubeToolboxReadyRunsOutputDelete(t *testing.T) {
	k, c := newKubeToolbox()
	if err := k.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := k.Create(context.Background(), toolboxSpec()); err != nil {
		t.Fatal(err)
	}
	runs, err := k.Runs(context.Background())
	if err != nil || len(runs) != 1 || runs[0].ActionID != toolboxSpec().ActionID {
		t.Fatalf("runs = %+v (%v)", runs, err)
	}
	if out, err := k.Output(context.Background(), toolboxSpec().ActionID, 4); err != nil || len(out) > 4 {
		t.Fatalf("output = %q (%v)", out, err)
	}
	if err := k.Delete(context.Background(), toolboxSpec().ActionID); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete(context.Background(), toolboxSpec().ActionID); err != nil {
		t.Fatalf("deleting a missing run: %v", err)
	}
	if runs, _ := k.Runs(context.Background()); len(runs) != 0 {
		t.Fatalf("runs after delete = %+v", runs)
	}

	c.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no"))
	})
	err = k.Ready(context.Background())
	if err == nil || !strings.Contains(err.Error(), "toolbox is not ready") {
		t.Fatalf("ready = %v", err)
	}
}

// A cluster-reach pod has the proxy as a native sidecar: only it mounts the run's credential (a
// second Secret) and the namespaces it must refuse are named; the script's container gets only the
// shared kubeconfig volume, read-only, and KUBECONFIG; neither has a ServiceAccount token. The
// admin token is minted with a lifetime past the pod's deadline and the Secret is deleted with the
// pod.
func TestKubeToolboxClusterPod(t *testing.T) {
	k, c := newKubeToolbox()
	k.Clients.Namespace = "mainloop-executor"
	var lifetime time.Duration
	k.AdminCredential = func(_ context.Context, d time.Duration) (privileged.KubeCredential, error) {
		lifetime = d
		return privileged.KubeCredential{Token: "tok-fictional-admin-1", Host: "https://10.96.0.1:443", CA: "ca-pem"}, nil
	}
	spec := toolboxSpec()
	spec.Reach, spec.ProxyImage = "cluster", proxyImage
	if err := k.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	const name = "exec-pa-0b6f1c2e-1111-4222-8333-444455556666"
	p, err := c.CoreV1().Pods("mainloop-toolbox").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := p.Spec
	if len(s.InitContainers) != 1 || s.InitContainers[0].RestartPolicy == nil || *s.InitContainers[0].RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Fatalf("init containers = %+v", s.InitContainers)
	}
	side, run := s.InitContainers[0], s.Containers[0]
	if side.Image != proxyImage || !slices.Equal(side.Command, []string{"/usr/local/bin/mainloop", "exec-kube-proxy"}) || run.Image != toolboxImage {
		t.Errorf("images: sidecar %s %v, run %s", side.Image, side.Command, run.Image)
	}
	env := map[string]string{}
	for _, e := range side.Env {
		env[e.Name] = e.Value
	}
	if env["MAINLOOP_EXEC_KUBE_PROTECT"] != "mainloop-executor,mainloop-toolbox" || env["MAINLOOP_EXEC_KUBE_CREDS"] != "/run/exec-kube" {
		t.Errorf("sidecar env = %v", env)
	}
	sc := side.SecurityContext
	if *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || !*sc.RunAsNonRoot || *sc.RunAsUser != 65532 || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Errorf("sidecar security = %+v", sc)
	}
	if side.StartupProbe == nil || side.StartupProbe.Exec == nil {
		t.Error("the script starts before the proxy is ready")
	}
	mounts := func(c corev1.Container, name string) *corev1.VolumeMount {
		for i := range c.VolumeMounts {
			if c.VolumeMounts[i].Name == name {
				return &c.VolumeMounts[i]
			}
		}
		return nil
	}
	if m := mounts(side, "kube-credential"); m == nil || !m.ReadOnly || mounts(run, "kube-credential") != nil {
		t.Errorf("the credential mount: sidecar %+v, run %+v", m, mounts(run, "kube-credential"))
	}
	if m := mounts(run, "kube"); m == nil || !m.ReadOnly || m.MountPath != "/run/kube" {
		t.Errorf("the script's kubeconfig mount = %+v", m)
	}
	if m := mounts(side, "kube"); m == nil || m.ReadOnly {
		t.Errorf("the proxy cannot write the kubeconfig: %+v", m)
	}
	for _, e := range run.Env {
		if e.Name == "KUBECONFIG" && e.Value != "/run/kube/kubeconfig" {
			t.Errorf("KUBECONFIG = %s", e.Value)
		}
		if strings.Contains(e.Value, "tok-fictional") {
			t.Errorf("the script's environment holds the token: %s", e.Name)
		}
	}
	if strings.Contains(strings.Join(run.Command, " "), "tok-fictional") || *s.AutomountServiceAccountToken {
		t.Error("the script's container is handed a credential")
	}
	if !strings.Contains(strings.Join(run.Command, " "), "/dev/termination-log") {
		t.Error("a missing proxy is not reported to the executor")
	}
	if lifetime <= time.Duration(*s.ActiveDeadlineSeconds)*time.Second {
		t.Errorf("token lifetime %s does not outlast the pod's deadline %ds", lifetime, *s.ActiveDeadlineSeconds)
	}
	sec, err := c.CoreV1().Secrets("mainloop-toolbox").Get(context.Background(), name+"-kube", metav1.GetOptions{})
	if err != nil || string(sec.Data["token"]) != "tok-fictional-admin-1" || string(sec.Data["host"]) != "https://10.96.0.1:443" || string(sec.Data["ca.crt"]) != "ca-pem" {
		t.Fatalf("credential secret = %+v (%v)", sec, err)
	}
	// An internet run has no sidecar, no credential and no KUBECONFIG.
	k2, c2 := newKubeToolbox()
	if err := k2.Create(context.Background(), toolboxSpec()); err != nil {
		t.Fatal(err)
	}
	p2, _ := c2.CoreV1().Pods("mainloop-toolbox").Get(context.Background(), name, metav1.GetOptions{})
	if len(p2.Spec.InitContainers) != 0 || strings.Contains(strings.Join(p2.Spec.Containers[0].Command, " "), "KUBECONFIG") {
		t.Errorf("an internet pod has the proxy: %+v", p2.Spec)
	}
	if _, err := c2.CoreV1().Secrets("mainloop-toolbox").Get(context.Background(), name+"-kube", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("an internet run has a credential Secret: %v", err)
	}
	// Deleting the run removes both Secrets; a refused pod leaves none.
	if err := k.Delete(context.Background(), spec.ActionID); err != nil {
		t.Fatal(err)
	}
	if l, _ := c.CoreV1().Secrets("mainloop-toolbox").List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
		t.Errorf("secrets left: %d", len(l.Items))
	}
	k3, c3 := newKubeToolbox()
	k3.AdminCredential = k.AdminCredential
	c3.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", errors.New("no"))
	})
	if err := k3.Create(context.Background(), spec); err == nil {
		t.Fatal("a refused pod was created")
	}
	if l, _ := c3.CoreV1().Secrets("mainloop-toolbox").List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
		t.Errorf("a refused run left %d Secrets", len(l.Items))
	}
}

var policyGVRTest = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}

// A run with secrets has the pinned reach label, which no deployment policy opens; its own
// CiliumNetworkPolicy allows DNS for its hosts and TCP 443 to them and nothing else, and selects
// its pod alone; its values are a Secret mounted read-only at /run/secrets and never an
// environment variable; deleting the run removes the policy and the Secret.
func TestKubeToolboxSecretsPod(t *testing.T) {
	k, c := newKubeToolbox()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{policyGVRTest: "CiliumNetworkPolicyList"})
	k.Dynamic, k.Policies = dyn, true
	spec := toolboxSpec()
	spec.Secrets = map[string][]byte{"cf": []byte("tok-fictional-cf-1")}
	spec.Hosts = []string{"api.cloudflare.com", "example.org"}
	if err := k.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	const name = "exec-pa-0b6f1c2e-1111-4222-8333-444455556666"
	p, err := c.CoreV1().Pods("mainloop-toolbox").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Labels["mainloop.dev/reach"] != "internet-pinned" {
		t.Errorf("reach label = %q: a deployment's internet policy would open it to every address", p.Labels["mainloop.dev/reach"])
	}
	run := p.Spec.Containers[0]
	var mount *corev1.VolumeMount
	for i := range run.VolumeMounts {
		if run.VolumeMounts[i].MountPath == "/run/secrets" {
			mount = &run.VolumeMounts[i]
		}
	}
	if mount == nil || !mount.ReadOnly {
		t.Errorf("secrets mount = %+v", mount)
	}
	for _, e := range run.Env {
		if strings.Contains(e.Value, "tok-fictional") {
			t.Errorf("a value is in the environment: %s", e.Name)
		}
	}
	sec, err := c.CoreV1().Secrets("mainloop-toolbox").Get(context.Background(), name+"-secrets", metav1.GetOptions{})
	if err != nil || string(sec.Data["cf"]) != "tok-fictional-cf-1" {
		t.Fatalf("secrets secret = %+v (%v)", sec, err)
	}
	got, err := k.RunSecrets(context.Background(), spec.ActionID)
	if err != nil || string(got["cf"]) != "tok-fictional-cf-1" {
		t.Fatalf("RunSecrets = %v (%v)", got, err)
	}
	pol, err := dyn.Resource(policyGVRTest).Namespace("mainloop-toolbox").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spc := pol.Object["spec"].(map[string]any)
	sel := spc["endpointSelector"].(map[string]any)["matchLabels"].(map[string]any)
	if len(sel) != 1 || sel["mainloop.dev/privileged-action"] != spec.ActionID {
		t.Errorf("the policy selects %v", sel)
	}
	egress := spc["egress"].([]any)
	if len(egress) != 2 {
		t.Fatalf("egress = %v", egress)
	}
	dns := egress[0].(map[string]any)["toPorts"].([]any)[0].(map[string]any)["rules"].(map[string]any)["dns"].([]any)
	fq := egress[1].(map[string]any)["toFQDNs"].([]any)
	ports := egress[1].(map[string]any)["toPorts"].([]any)[0].(map[string]any)["ports"].([]any)
	if len(dns) != 2 || len(fq) != 2 || fq[0].(map[string]any)["matchName"] != "api.cloudflare.com" || len(ports) != 1 || ports[0].(map[string]any)["port"] != "443" {
		t.Errorf("policy = %v", spc)
	}
	if _, ok := egress[1].(map[string]any)["toCIDR"]; ok {
		t.Error("the policy has a CIDR rule")
	}
	if err := k.Delete(context.Background(), spec.ActionID); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(policyGVRTest).Namespace("mainloop-toolbox").Get(context.Background(), name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the policy is left: %v", err)
	}
	if l, _ := c.CoreV1().Secrets("mainloop-toolbox").List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
		t.Errorf("secrets left: %d", len(l.Items))
	}
	// A run without secrets has none of this.
	k2, c2 := newKubeToolbox()
	if err := k2.Create(context.Background(), toolboxSpec()); err != nil {
		t.Fatal(err)
	}
	p2, _ := c2.CoreV1().Pods("mainloop-toolbox").Get(context.Background(), name, metav1.GetOptions{})
	if p2.Labels["mainloop.dev/reach"] != "internet" {
		t.Errorf("reach label = %q", p2.Labels["mainloop.dev/reach"])
	}
}
