// Package deploytest renders the Helm chart (charts/mainloop) offline and checks the properties
// the Go server and React app depend on. No cluster is contacted.
package deploytest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const chart = "../../../charts/mainloop"

// installs are the values files a deployment of the chart is rendered with. The production and
// live-dev files are generic stand-ins; real deployments keep theirs in their own repository.
var installs = map[string]struct {
	namespace, values string
	set               []string
}{
	"defaults":   {"mainloop", "", nil},
	"production": {"mainloop", "ci/production-values.yaml", nil},
	"live-dev":   {"mainloop-dev", "ci/live-dev-values.yaml", nil},
	"kind":       {"mainloop", "values-kind.yaml", nil},
	// Production with its canary on, as the real production values have it.
	"production-canary": {"mainloop", "ci/production-values.yaml", []string{"canary.enabled=true"}},
	// Production with the preview screenshot browser on.
	"production-screenshots": {"mainloop", "ci/production-values.yaml", []string{
		"screenshots.enabled=true",
		"screenshots.previewEgress[0].namespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=gateway-system",
		"screenshots.previewEgress[0].podSelector.matchLabels.app=preview-gateway",
	}},
}

// rendered is one install after `helm template`, split by kind.
type rendered struct {
	deployments  map[string]appsv1.Deployment
	statefulSets map[string]appsv1.StatefulSet
	configMaps   map[string]corev1.ConfigMap
	services     map[string]corev1.Service
	policies     []networkingv1.NetworkPolicy
	clusterRoles []rbacv1.ClusterRole
	bindings     []rbacv1.ClusterRoleBinding
	roleBindings []rbacv1.RoleBinding
	cronJobs     map[string]batchv1.CronJob
	accounts     map[string]corev1.ServiceAccount
	// objects is "Kind/name" for every object, so a test can assert what is absent.
	objects []string
}

// render templates the chart with the local helm (client-side, offline).
func render(t *testing.T, path string) rendered {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	install, ok := installs[path]
	if !ok {
		t.Fatalf("no install %q", path)
	}
	args := []string{"template", "mainloop", chart, "--namespace", install.namespace}
	if install.values != "" {
		args = append(args, "--values", filepath.Join(chart, install.values))
	}
	for _, kv := range install.set {
		args = append(args, "--set", kv)
	}
	cmd := exec.CommandContext(t.Context(), helm, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template %s: %v\n%s", path, err, stderr.String())
	}

	r := rendered{
		deployments:  map[string]appsv1.Deployment{},
		statefulSets: map[string]appsv1.StatefulSet{},
		configMaps:   map[string]corev1.ConfigMap{},
		services:     map[string]corev1.Service{},
		cronJobs:     map[string]batchv1.CronJob{},
		accounts:     map[string]corev1.ServiceAccount{},
	}
	docs := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(out)))
	for {
		doc, err := docs.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("split %s: %v", path, err)
		}
		var raw json.RawMessage
		if err := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(doc)).Decode(&raw); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil || meta.Kind == "" {
			continue
		}
		name := meta.Metadata.Name
		r.objects = append(r.objects, meta.Kind+"/"+name)
		switch meta.Kind {
		case "Deployment":
			var d appsv1.Deployment
			mustUnmarshal(t, raw, &d)
			r.deployments[name] = d
		case "StatefulSet":
			var s appsv1.StatefulSet
			mustUnmarshal(t, raw, &s)
			r.statefulSets[name] = s
		case "ConfigMap":
			var c corev1.ConfigMap
			mustUnmarshal(t, raw, &c)
			r.configMaps[name] = c
		case "Service":
			var s corev1.Service
			mustUnmarshal(t, raw, &s)
			r.services[name] = s
		case "NetworkPolicy":
			var p networkingv1.NetworkPolicy
			mustUnmarshal(t, raw, &p)
			r.policies = append(r.policies, p)
		case "ClusterRole":
			var c rbacv1.ClusterRole
			mustUnmarshal(t, raw, &c)
			r.clusterRoles = append(r.clusterRoles, c)
		case "ClusterRoleBinding":
			var b rbacv1.ClusterRoleBinding
			mustUnmarshal(t, raw, &b)
			r.bindings = append(r.bindings, b)
		case "RoleBinding":
			var b rbacv1.RoleBinding
			mustUnmarshal(t, raw, &b)
			r.roleBindings = append(r.roleBindings, b)
		case "CronJob":
			var c batchv1.CronJob
			mustUnmarshal(t, raw, &c)
			r.cronJobs[name] = c
		case "ServiceAccount":
			var a corev1.ServiceAccount
			mustUnmarshal(t, raw, &a)
			r.accounts[name] = a
		}
	}
	return r
}

func mustUnmarshal(t *testing.T, raw []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}

func containers(pod corev1.PodSpec) map[string]corev1.Container {
	out := map[string]corev1.Container{}
	for _, c := range pod.Containers {
		out[c.Name] = c
	}
	return out
}

func envByName(c corev1.Container) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		out[e.Name] = e
	}
	return out
}

func (r rendered) backend(t *testing.T) corev1.PodSpec {
	t.Helper()
	d, ok := r.deployments["mainloop-backend"]
	if !ok {
		t.Fatal("no mainloop-backend Deployment")
	}
	return d.Spec.Template.Spec
}

// serverContainers is every container that runs the Go binary, init containers included.
func serverContainers(pod corev1.PodSpec) []corev1.Container {
	var out []corev1.Container
	for _, c := range append(slices.Clone(pod.InitContainers), pod.Containers...) {
		if c.Name == "kagent-control-trust-bundle" {
			continue
		}
		out = append(out, c)
	}
	return out
}

var installNames = []string{"defaults", "production", "live-dev", "kind"}

func TestServerContainersRunTheGoBinary(t *testing.T) {
	for _, path := range installNames {
		t.Run(path, func(t *testing.T) {
			pod := render(t, path).backend(t)
			for _, c := range serverContainers(pod) {
				argv := strings.ToLower(strings.Join(append(slices.Clone(c.Command), c.Args...), " "))
				for _, banned := range []string{"python", "uvicorn", "uv run", ".venv", "mainloop.api", "mainloop.mcp_app", "mainloop.git_app"} {
					if strings.Contains(argv, banned) {
						t.Errorf("%s: %q still runs the Python stack: %s", c.Name, banned, argv)
					}
				}
				if !strings.Contains(argv, "mainloop") {
					t.Errorf("%s: command does not run mainloop: %s", c.Name, argv)
				}
			}
			got := map[string]bool{}
			for _, c := range pod.Containers {
				got[c.Name] = true
			}
			for _, name := range []string{"backend", "mcp", "git"} {
				if !got[name] {
					t.Errorf("missing container %s", name)
				}
			}
		})
	}
}

func TestServeSubcommandsAndPorts(t *testing.T) {
	want := map[string]struct {
		subcommand string
		ports      []int32
	}{
		"backend": {"serve-api", []int32{8000, 9100}},
		"mcp":     {"serve-mcp", []int32{8002}},
		"git":     {"serve-git", []int32{8003, 8004}},
	}
	for _, path := range installNames {
		t.Run(path, func(t *testing.T) {
			cs := containers(render(t, path).backend(t))
			for name, w := range want {
				c := cs[name]
				argv := append(slices.Clone(c.Command), c.Args...)
				if !slices.Contains(argv, w.subcommand) {
					t.Errorf("%s: argv %v lacks %s", name, argv, w.subcommand)
				}
				var ports []int32
				for _, p := range c.Ports {
					ports = append(ports, p.ContainerPort)
				}
				if !slices.Equal(ports, w.ports) {
					t.Errorf("%s: ports %v, want %v", name, ports, w.ports)
				}
			}
		})
	}
}

func TestProbesAreHTTPOrTCP(t *testing.T) {
	for _, path := range installNames {
		t.Run(path, func(t *testing.T) {
			r := render(t, path)
			for name, d := range r.deployments {
				pod := d.Spec.Template.Spec
				for _, c := range append(slices.Clone(pod.InitContainers), pod.Containers...) {
					for kind, p := range map[string]*corev1.Probe{"liveness": c.LivenessProbe, "readiness": c.ReadinessProbe, "startup": c.StartupProbe} {
						if p == nil {
							continue
						}
						if p.Exec != nil {
							t.Errorf("%s/%s: %s probe execs a command", name, c.Name, kind)
						}
						if p.HTTPGet == nil && p.TCPSocket == nil {
							t.Errorf("%s/%s: %s probe is neither httpGet nor tcpSocket", name, c.Name, kind)
						}
					}
				}
			}
			for name, c := range containers(r.backend(t)) {
				if c.ReadinessProbe == nil || c.LivenessProbe == nil {
					t.Errorf("%s: missing a liveness or readiness probe", name)
				}
			}
		})
	}
}

// A startup probe is opt-in (server.startupProbe): the default install starts a prebuilt binary
// and needs none.
// The backend is one replica, so when its node is drained it must be able to preempt an ordinary
// app instead of staying Pending: a production values file names a PriorityClass and the pod gets
// it. The chart leaves it unset by default, since the class is cluster-wide and not the chart's.
func TestBackendPriorityClass(t *testing.T) {
	for path, want := range map[string]string{"defaults": "", "kind": "", "production": "mainloop-critical"} {
		t.Run(path, func(t *testing.T) {
			r := render(t, path)
			if got := r.backend(t).PriorityClassName; got != want {
				t.Errorf("backend priorityClassName = %q, want %q", got, want)
			}
			for _, o := range r.objects {
				if strings.HasPrefix(o, "PriorityClass/") {
					t.Errorf("chart renders %s; the PriorityClass belongs to the cluster owner", o)
				}
			}
		})
	}
}

func TestNoStartupProbeByDefault(t *testing.T) {
	for _, path := range []string{"defaults", "production"} {
		t.Run(path, func(t *testing.T) {
			for name, c := range containers(render(t, path).backend(t)) {
				if c.StartupProbe != nil {
					t.Errorf("%s: startup probe %+v, want none", name, c.StartupProbe)
				}
			}
		})
	}
}

// Liveness restarts a container, so it must only fail when the process itself is stuck: the
// API's /health pings the database, and as a liveness probe a database outage would restart every
// API container (and keep it in crash-loop backoff after the database is back). Readiness keeps
// /health, which takes the pod out of its Services while the database is down.
func TestLivenessDoesNotDependOnTheDatabase(t *testing.T) {
	for _, path := range installNames {
		t.Run(path, func(t *testing.T) {
			cs := containers(render(t, path).backend(t))
			if api := cs["backend"].LivenessProbe; api == nil || api.TCPSocket == nil || api.HTTPGet != nil {
				t.Errorf("backend liveness = %+v, want a tcpSocket probe (its /health pings the database)", api)
			}
			if api := cs["backend"].ReadinessProbe; api == nil || api.HTTPGet == nil || api.HTTPGet.Path != "/health" {
				t.Errorf("backend readiness = %+v, want GET /health", api)
			}
		})
	}
}

func TestMigrateInitContainerRunsFirst(t *testing.T) {
	for _, path := range installNames {
		t.Run(path, func(t *testing.T) {
			pod := render(t, path).backend(t)
			var migrate *corev1.Container
			for i := range pod.InitContainers {
				if pod.InitContainers[i].Name == "migrate" {
					migrate = &pod.InitContainers[i]
				}
			}
			if migrate == nil {
				t.Fatal("no migrate initContainer")
			}
			argv := slices.Concat(migrate.Command, migrate.Args)
			if n := len(argv); n < 2 || argv[n-2] != "migrate" || argv[n-1] != "up" {
				t.Errorf("migrate argv %v does not end in `migrate up`", argv)
			}
			env := envByName(*migrate)
			for _, name := range []string{"DB_USER", "DB_PASSWORD"} {
				if env[name].ValueFrom == nil || env[name].ValueFrom.SecretKeyRef == nil {
					t.Errorf("migrate: %s is not a secretKeyRef", name)
				}
			}
			if len(migrate.EnvFrom) == 0 {
				t.Error("migrate: no mainloop-config envFrom for DB_HOST, DB_NAME")
			}
			if migrate.Image != containers(pod)["backend"].Image {
				t.Errorf("migrate image %s differs from backend image %s", migrate.Image, containers(pod)["backend"].Image)
			}
		})
	}
}

func TestSecretsAreReferencedNotEmbedded(t *testing.T) {
	secrets := []string{"DB_PASSWORD", "AGENT_TOKEN_KEY", "GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY"}
	for _, path := range installNames {
		t.Run(path, func(t *testing.T) {
			for _, c := range serverContainers(render(t, path).backend(t)) {
				env := envByName(c)
				if _, ok := env["GITHUB_TOKEN"]; ok {
					t.Errorf("%s: carries GITHUB_TOKEN; the server uses GitHub App credentials", c.Name)
				}
				for _, name := range secrets {
					if e, ok := env[name]; ok && e.Value != "" {
						t.Errorf("%s: %s is a literal value", c.Name, name)
					}
				}
			}
		})
	}
}

// Every install but the live dev instance requires the GitHub App keys from mainloop-secrets.
func TestGitHubAppKeysComeFromMainloopSecrets(t *testing.T) {
	for _, path := range installNames {
		t.Run(path, func(t *testing.T) {
			for name, c := range containers(render(t, path).backend(t)) {
				env := envByName(c)
				for envName, key := range map[string]string{"GITHUB_APP_ID": "github-app-id", "GITHUB_APP_PRIVATE_KEY": "github-app-private-key"} {
					ref := env[envName].ValueFrom
					if ref == nil || ref.SecretKeyRef == nil {
						t.Errorf("%s: %s is not a secretKeyRef", name, envName)
						continue
					}
					if ref.SecretKeyRef.Name != "mainloop-secrets" || ref.SecretKeyRef.Key != key {
						t.Errorf("%s: %s refs %s/%s", name, envName, ref.SecretKeyRef.Name, ref.SecretKeyRef.Key)
					}
					if optional := ref.SecretKeyRef.Optional != nil && *ref.SecretKeyRef.Optional; optional != (path == "live-dev") {
						t.Errorf("%s: %s optional=%v in %s", name, envName, optional, path)
					}
				}
				// The server has no fallback for the token key, so it is never optional.
				ref := env["AGENT_TOKEN_KEY"].ValueFrom
				if ref == nil || ref.SecretKeyRef == nil || (ref.SecretKeyRef.Optional != nil && *ref.SecretKeyRef.Optional) {
					t.Errorf("%s: AGENT_TOKEN_KEY must be a required secretKeyRef", name)
				}
			}
		})
	}
}

func TestGitListenersAreInternalAndShareTheBackendEnvironment(t *testing.T) {
	for _, path := range installNames {
		t.Run(path, func(t *testing.T) {
			r := render(t, path)
			pod := r.backend(t)
			cs := containers(pod)
			git, backend := cs["git"], cs["backend"]
			if git.Image != backend.Image {
				t.Errorf("git image %s != backend image %s", git.Image, backend.Image)
			}
			gitEnv, backendEnv := envByName(git), envByName(backend)
			for _, name := range []string{"DB_USER", "DB_PASSWORD", "AGENT_TOKEN_KEY"} {
				if !envEqual(gitEnv[name], backendEnv[name]) {
					t.Errorf("git %s differs from backend", name)
				}
			}
			if git.SecurityContext == nil || git.SecurityContext.RunAsNonRoot == nil || !*git.SecurityContext.RunAsNonRoot ||
				git.SecurityContext.AllowPrivilegeEscalation == nil || *git.SecurityContext.AllowPrivilegeEscalation {
				t.Error("git must run as non-root without privilege escalation")
			}
			if !slices.ContainsFunc(git.VolumeMounts, func(m corev1.VolumeMount) bool {
				return m.Name == "git-spool" && m.MountPath == "/var/lib/mainloop/git" && !m.ReadOnly
			}) {
				t.Errorf("git spool mount: %+v", git.VolumeMounts)
			}
			var spool *corev1.Volume
			for i := range pod.Volumes {
				if pod.Volumes[i].Name == "git-spool" {
					spool = &pod.Volumes[i]
				}
			}
			if spool == nil || spool.EmptyDir == nil || spool.EmptyDir.SizeLimit == nil || spool.EmptyDir.SizeLimit.String() != "3Gi" {
				t.Errorf("git-spool volume: %+v", spool)
			}
			if pod.SecurityContext == nil || pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 1000 {
				t.Error("pod fsGroup must be 1000 so the git spool is writable")
			}
			for purpose, port := range map[string]int32{"read": 8003, "push": 8004} {
				s, ok := r.services["mainloop-git-"+purpose]
				if !ok {
					t.Errorf("no mainloop-git-%s Service", purpose)
					continue
				}
				if s.Spec.Type != "" && s.Spec.Type != corev1.ServiceTypeClusterIP {
					t.Errorf("%s: type %s, want ClusterIP", s.Name, s.Spec.Type)
				}
				if len(s.Spec.Selector) != 1 || s.Spec.Selector["app"] != "mainloop-backend" {
					t.Errorf("%s: selector %v", s.Name, s.Spec.Selector)
				}
				if len(s.Spec.Ports) != 1 || s.Spec.Ports[0].Port != 80 || s.Spec.Ports[0].TargetPort != intstr.FromInt32(port) {
					t.Errorf("%s: ports %+v", s.Name, s.Spec.Ports)
				}
			}
		})
	}
}

func envEqual(a, b corev1.EnvVar) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// The git ports accept traffic only from the egress proxy, and no NetworkPolicy adds another peer.
func TestNoNetworkPolicyOpensGitPortsToAnotherPeer(t *testing.T) {
	for _, path := range installNames {
		t.Run(path, func(t *testing.T) {
			allowed := map[intstr.IntOrString]bool{}
			for _, policy := range render(t, path).policies {
				sel := policy.Spec.PodSelector.MatchLabels["app"]
				if sel != "" && sel != "mainloop-backend" {
					continue
				}
				for _, rule := range policy.Spec.Ingress {
					if len(rule.Ports) == 0 {
						t.Errorf("%s: unbounded ingress opens both git listeners", policy.Name)
					}
					for _, p := range rule.Ports {
						if p.Port == nil || !slices.Contains([]string{"8003", "8004", "git-read", "git-push"}, p.Port.String()) {
							continue
						}
						allowed[*p.Port] = true
						if len(rule.From) != 1 {
							t.Errorf("%s: git port %s has %d peers", policy.Name, p.Port, len(rule.From))
							continue
						}
						from := rule.From[0]
						if from.NamespaceSelector == nil || from.PodSelector == nil ||
							from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "ate-system" ||
							from.PodSelector.MatchLabels["app"] != "atenet-egress" {
							t.Errorf("%s: git port %s peer is %+v", policy.Name, p.Port, from)
						}
						if p.Protocol != nil && *p.Protocol != corev1.ProtocolTCP {
							t.Errorf("%s: git port %s protocol %s", policy.Name, p.Port, *p.Protocol)
						}
					}
				}
			}
			if len(allowed) != 2 {
				t.Errorf("git ports allowed by policy: %v, want both listeners", allowed)
			}
		})
	}
}

func TestNoClusterScopedSecretAccess(t *testing.T) {
	grants := func(rule rbacv1.PolicyRule) bool {
		return (slices.Contains(rule.Resources, "secrets") || slices.Contains(rule.Resources, "*")) && len(rule.Verbs) > 0
	}
	for _, path := range installNames {
		t.Run(path, func(t *testing.T) {
			r := render(t, path)
			defined := map[string]bool{}
			for _, role := range r.clusterRoles {
				defined[role.Name] = true
				for _, rule := range role.Rules {
					if grants(rule) {
						t.Errorf("ClusterRole %s reads Secrets cluster-wide", role.Name)
					}
				}
			}
			// A binding to a role defined elsewhere (cluster-admin, a built-in) would bypass the check.
			for _, b := range r.bindings {
				if !defined[b.RoleRef.Name] || slices.Contains([]string{"cluster-admin", "admin", "edit"}, b.RoleRef.Name) {
					t.Errorf("ClusterRoleBinding %s binds %s", b.Name, b.RoleRef.Name)
				}
			}
		})
	}
	t.Run("the check catches a Secret rule", func(t *testing.T) {
		if !grants(rbacv1.PolicyRule{Resources: []string{"secrets"}, Verbs: []string{"get"}}) ||
			!grants(rbacv1.PolicyRule{Resources: []string{"*"}, Verbs: []string{"*"}}) ||
			grants(rbacv1.PolicyRule{Resources: []string{"pods"}, Verbs: []string{"get"}}) {
			t.Fatal("grants() misclassifies a rule")
		}
	})
}

func TestFeatureFlagsDefaultOff(t *testing.T) {
	flags := []string{"GIT_TRANSPORT_ENABLED", "PUSH_GATE_ENABLED", "MAINLOOP_MERGE_TOOLS_ENABLED", "MAINLOOP_OWNER_HITL_WRITES_ENABLED", "MAINLOOP_OWNER_POLICY_WRITES_ENABLED", "MAINLOOP_PRIVILEGED_ACTIONS_ENABLED"}
	// The live dev instance is a single-owner prototype, so it may turn HITL answers, Git transport
	// and the push gate on (through its own proxies). Every other install keeps every flag off.
	allowed := map[string][]string{
		"live-dev": {"GIT_TRANSPORT_ENABLED", "PUSH_GATE_ENABLED", "MAINLOOP_OWNER_HITL_WRITES_ENABLED"},
	}
	for _, path := range installNames {
		t.Run(path, func(t *testing.T) {
			data := render(t, path).configMaps["mainloop-config"].Data
			for _, flag := range flags {
				if slices.Contains(allowed[path], flag) {
					continue
				}
				if v := strings.ToLower(data[flag]); v != "" && v != "false" {
					t.Errorf("%s=%s", flag, data[flag])
				}
			}
		})
	}
}

func TestProdIsNotTheLiveDevInstance(t *testing.T) {
	data := render(t, "production").configMaps["mainloop-config"].Data
	if data["MAINLOOP_ALLOW_RESET"] != "" {
		t.Error("prod must not allow `migrate reset`")
	}
	for _, c := range render(t, "production").backend(t).Containers {
		if strings.Contains(c.Image, "-dev") {
			t.Errorf("prod container %s runs dev image %s", c.Name, c.Image)
		}
	}
}

func TestLiveDev(t *testing.T) {
	r := render(t, "live-dev")
	pod := r.backend(t)

	t.Run("images are the arm64 main dev images", func(t *testing.T) {
		const server, web = "ghcr.io/oldsj/mainloop-server-dev:main", "ghcr.io/oldsj/mainloop-web-dev:main"
		for _, c := range append(slices.Clone(pod.InitContainers), pod.Containers...) {
			if c.Image != server {
				t.Errorf("%s: image %s, want %s", c.Name, c.Image, server)
			}
		}
		frontend := r.deployments["mainloop-frontend"].Spec.Template.Spec
		for _, c := range frontend.Containers {
			if c.Image != web {
				t.Errorf("frontend %s: image %s, want %s", c.Name, c.Image, web)
			}
		}
		for name, spec := range map[string]corev1.PodSpec{"backend": pod, "frontend": frontend} {
			if spec.NodeSelector["kubernetes.io/arch"] != "arm64" {
				t.Errorf("%s: nodeSelector %v, want arm64", name, spec.NodeSelector)
			}
			if len(spec.ImagePullSecrets) != 0 {
				t.Errorf("%s: dev images are public but a pull secret is set", name)
			}
		}
	})

	t.Run("server containers watch and rebuild from /src", func(t *testing.T) {
		for name, sub := range map[string]string{"backend": "serve-api", "mcp": "serve-mcp", "git": "serve-git"} {
			c := containers(pod)[name]
			argv := strings.Join(slices.Concat(c.Command, c.Args), " ")
			want := "watchexec --restart --exts go,sql --stop-timeout 5s -- go run ./cmd/mainloop " + sub
			if argv != want {
				t.Errorf("%s: argv %q, want %q", name, argv, want)
			}
			if c.WorkingDir != "/src" {
				t.Errorf("%s: workingDir %q, want /src", name, c.WorkingDir)
			}
		}
	})

	t.Run("a startup probe covers the cold go run build", func(t *testing.T) {
		for name, c := range containers(pod) {
			s, l := c.StartupProbe, c.LivenessProbe
			if s == nil || l == nil {
				t.Errorf("%s: startup %+v, liveness %+v, want both", name, s, l)
				continue
			}
			if !reflect.DeepEqual(s.ProbeHandler, l.ProbeHandler) {
				t.Errorf("%s: startup handler %+v, want the liveness handler %+v", name, s.ProbeHandler, l.ProbeHandler)
			}
			if budget := s.PeriodSeconds * s.FailureThreshold; budget < 300 {
				t.Errorf("%s: startup budget %ds, want at least 300s for a cold build", name, budget)
			}
		}
	})

	t.Run("frontend runs Vite and proxies /api to the backend Service", func(t *testing.T) {
		c := containers(r.deployments["mainloop-frontend"].Spec.Template.Spec)["frontend"]
		if !strings.HasSuffix(strings.Join(c.Command, " "), "vite") {
			t.Errorf("frontend command %v does not run vite", c.Command)
		}
		env := envByName(c)
		if env["MAINLOOP_API_PROXY"].Value != "http://mainloop-backend:8000" {
			t.Errorf("MAINLOOP_API_PROXY=%q", env["MAINLOOP_API_PROXY"].Value)
		}
		if env["VITE_API_URL"].Value != "/api" || env["VITE_HOST"].Value != "0.0.0.0" || env["VITE_PORT"].Value != "3000" {
			t.Errorf("vite env: %+v", env)
		}
		if env["__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS"].Value != "mainloop-dev.example.test" {
			t.Errorf("allowed host: %q", env["__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS"].Value)
		}
	})

	t.Run("config", func(t *testing.T) {
		data := r.configMaps["mainloop-config"].Data
		if data["MAINLOOP_ALLOW_RESET"] != "1" {
			t.Errorf("MAINLOOP_ALLOW_RESET=%q, want exactly 1", data["MAINLOOP_ALLOW_RESET"])
		}
		hosts := strings.Split(data["MAINLOOP_MCP_HOSTS"], ",")
		if !slices.Contains(hosts, "mainloop-mcp.mainloop-dev.svc.cluster.local") {
			t.Errorf("MAINLOOP_MCP_HOSTS=%q lacks the in-cluster MCP host", data["MAINLOOP_MCP_HOSTS"])
		}
		if data["MAINLOOP_NAMESPACE"] != "mainloop-dev" {
			t.Errorf("MAINLOOP_NAMESPACE=%q", data["MAINLOOP_NAMESPACE"])
		}
		for key, v := range data {
			if strings.HasPrefix(key, "KAGENT_") && strings.HasSuffix(key, "_AGENT") && !strings.HasSuffix(v, "-dev") {
				t.Errorf("%s=%s: dev-live must use the *-dev agents", key, v)
			}
		}
	})

	t.Run("control-auth is enabled without Python", func(t *testing.T) {
		for _, name := range []string{"backend", "mcp", "git"} {
			env := envByName(containers(pod)[name])
			if env["KAGENT_USER_ID"].Value != "mainloop" || env["KAGENT_CONTROL_TOKEN_FILE"].Value != "/run/kagent-control-auth/token" ||
				env["SSL_CERT_FILE"].Value != "/run/kagent-control-ca/ca-bundle.crt" {
				t.Errorf("%s: control-auth env missing: %+v", name, env)
			}
		}
		// The bearer is a projected ServiceAccount token for kagent's audience, not a static Secret.
		var token *corev1.ServiceAccountTokenProjection
		for _, v := range pod.Volumes {
			if v.Name == "kagent-control-auth" && v.Projected != nil && len(v.Projected.Sources) == 1 {
				token = v.Projected.Sources[0].ServiceAccountToken
			}
			if v.Secret != nil && v.Secret.SecretName == "kagent-control-token" {
				t.Errorf("volume %s still mounts the static kagent-control-token Secret", v.Name)
			}
		}
		if token == nil || token.Audience != "kagent-invocation" || token.Path != "token" {
			t.Errorf("kagent-control-auth is not a projected ServiceAccount token for kagent-invocation: %+v", token)
		}
		var bundle *corev1.Container
		for i := range pod.InitContainers {
			if pod.InitContainers[i].Name == "kagent-control-trust-bundle" {
				bundle = &pod.InitContainers[i]
			}
		}
		if bundle == nil {
			t.Fatal("no kagent-control-trust-bundle initContainer")
		}
		script := strings.Join(slices.Concat(bundle.Command, bundle.Args), " ")
		for _, banned := range []string{"python", ".venv", "certifi.where"} {
			if strings.Contains(script, banned) {
				t.Errorf("trust bundle still uses %s", banned)
			}
		}
		if !strings.Contains(script, "/run/kagent-control-root/ca.crt") || !strings.Contains(script, "/run/kagent-control-ca/ca-bundle.crt") {
			t.Errorf("trust bundle script does not build the bundle: %s", script)
		}
	})
}

// The Kind install (`make dev`) runs the same Go images as prod against a local PostgreSQL. It must
// boot without kagent, the GitHub App or a registry pull, and its Services and origin must match
// the port forwards in devspace.yaml and the Makefile.
func TestKindBootsWithoutKagentOrGitHubApp(t *testing.T) {
	r := render(t, "kind")

	for _, o := range r.objects {
		if lower := strings.ToLower(o); strings.Contains(lower, "kagent") || strings.Contains(lower, "substrate") {
			t.Errorf("%s: the local stack must not depend on kagent or Substrate", o)
		}
	}
	if n := len(r.clusterRoles) + len(r.bindings) + len(r.roleBindings); n != 0 {
		t.Errorf("Kind renders %d RBAC objects; the local stack needs none", n)
	}

	// Reached only through port forwards, so nothing is exposed on the node.
	for name, s := range r.services {
		if s.Spec.Type != "" && s.Spec.Type != corev1.ServiceTypeClusterIP {
			t.Errorf("Service %s is %s, want ClusterIP", name, s.Spec.Type)
		}
	}
	for name, port := range map[string]int32{"mainloop-frontend": 3000, "mainloop-backend": 8000} {
		if !slices.ContainsFunc(r.services[name].Spec.Ports, func(p corev1.ServicePort) bool { return p.Port == port }) {
			t.Errorf("Service %s lacks port %d, which the port forwards target", name, port)
		}
	}

	// Images are built on the host and loaded into Kind, so nothing may pull from a registry.
	for name, d := range r.deployments {
		pod := d.Spec.Template.Spec
		if len(pod.ImagePullSecrets) != 0 {
			t.Errorf("%s: imagePullSecrets %v; Kind has no ghcr pull secret", name, pod.ImagePullSecrets)
		}
		for _, c := range append(slices.Clone(pod.InitContainers), pod.Containers...) {
			if want := "mainloop-" + strings.TrimPrefix(name, "mainloop-") + ":kind"; c.Image != want {
				t.Errorf("%s/%s: image %s, want %s", name, c.Name, c.Image, want)
			}
			if c.ImagePullPolicy == corev1.PullAlways {
				t.Errorf("%s/%s: imagePullPolicy Always would bypass the loaded image", name, c.Name)
			}
		}
	}

	// The backend reaches the PostgreSQL StatefulSet through DB_HOST.
	data := r.configMaps["mainloop-config"].Data
	db, ok := r.statefulSets["mainloop-postgres"]
	if !ok {
		t.Fatal("no mainloop-postgres StatefulSet")
	}
	if data["DB_HOST"] != db.Spec.ServiceName {
		t.Errorf("DB_HOST %q, want the PostgreSQL Service %q", data["DB_HOST"], db.Spec.ServiceName)
	}
	dbService, ok := r.services[db.Spec.ServiceName]
	if !ok {
		t.Fatalf("no Service %s for the PostgreSQL StatefulSet", db.Spec.ServiceName)
	}
	if !maps.Equal(dbService.Spec.Selector, db.Spec.Selector.MatchLabels) {
		t.Errorf("PostgreSQL Service selector %v does not match the StatefulSet %v", dbService.Spec.Selector, db.Spec.Selector.MatchLabels)
	}
	if data["DB_PORT"] != "5432" {
		t.Errorf("DB_PORT %q, want 5432", data["DB_PORT"])
	}

	// The API's Origin check compares these exact values, so they must match the port forward.
	origin := data["FRONTEND_SCHEME"] + "://" + data["FRONTEND_DOMAIN"]
	if origin != "http://localhost:5173" {
		t.Errorf("origin %s, want http://localhost:5173 to match the port forward", origin)
	}
	frontend := containers(r.deployments["mainloop-frontend"].Spec.Template.Spec)["frontend"]
	if got := envByName(frontend)["ORIGIN"].Value; got != origin {
		t.Errorf("frontend ORIGIN %q, want %q", got, origin)
	}

	// `make dev-reset` wipes this database, and only this one.
	if data["MAINLOOP_ALLOW_RESET"] != "1" {
		t.Errorf("MAINLOOP_ALLOW_RESET %q, want 1 for dev-reset", data["MAINLOOP_ALLOW_RESET"])
	}
}

// The preview proxy and the live relay reach the Substrate 1.0 router over mutual TLS: the API
// container, and only it, gets a podidentity pod certificate and the servicedns trust bundle at
// the paths the server reads by default.
func TestTheAPIGetsTheSubstrateRouterCredentials(t *testing.T) {
	for _, path := range []string{"production", "live-dev"} {
		t.Run(path, func(t *testing.T) {
			r := render(t, path)
			if got := r.configMaps["mainloop-config"].Data["SUBSTRATE_ROUTER_ADDRESS"]; got != "" &&
				got != "https://atenet-router.ate-system.svc.cluster.local:8444" {
				t.Errorf("SUBSTRATE_ROUTER_ADDRESS=%q, want the router's CONNECT-over-TLS listener", got)
			}
			pod := r.backend(t)
			var cert *corev1.PodCertificateProjection
			var bundle *corev1.ClusterTrustBundleProjection
			for _, v := range pod.Volumes {
				if v.Projected == nil || len(v.Projected.Sources) != 1 {
					continue
				}
				switch v.Name {
				case "substrate-router-identity":
					cert = v.Projected.Sources[0].PodCertificate
				case "substrate-router-ca":
					bundle = v.Projected.Sources[0].ClusterTrustBundle
				}
			}
			if cert == nil || cert.SignerName != "podidentity.podcert.ate.dev/identity" || cert.CredentialBundlePath != "credential-bundle.pem" {
				t.Errorf("substrate-router-identity is not a podidentity pod certificate: %+v", cert)
			}
			if bundle == nil || bundle.SignerName == nil || *bundle.SignerName != "servicedns.podcert.ate.dev/identity" || bundle.Path != "trust-bundle.pem" {
				t.Errorf("substrate-router-ca is not the servicedns trust bundle: %+v", bundle)
			}
			for name, c := range containers(pod) {
				mounts := map[string]string{}
				for _, m := range c.VolumeMounts {
					mounts[m.Name] = m.MountPath
				}
				want := name == "backend"
				if got := mounts["substrate-router-identity"] == "/run/substrate-router/identity" &&
					mounts["substrate-router-ca"] == "/run/substrate-router/ca"; got != want {
					t.Errorf("%s: router credentials mounted = %v, want %v (%v)", name, got, want, mounts)
				}
			}
		})
	}
}

// apiToken checks that pod carries a projected token for the owner API's audience, mounted where
// container's MAINLOOP_API_TOKEN_FILE points, and returns its lifetime.
func apiToken(t *testing.T, pod corev1.PodSpec, c corev1.Container) int64 {
	t.Helper()
	var src *corev1.ServiceAccountTokenProjection
	for _, v := range pod.Volumes {
		if v.Name == "mainloop-api-token" && v.Projected != nil && len(v.Projected.Sources) == 1 {
			src = v.Projected.Sources[0].ServiceAccountToken
		}
	}
	if src == nil || src.Audience != "mainloop-api" || src.ExpirationSeconds == nil {
		t.Fatalf("no projected mainloop-api token volume: %+v", pod.Volumes)
	}
	file := envByName(c)["MAINLOOP_API_TOKEN_FILE"].Value
	for _, m := range c.VolumeMounts {
		if m.Name == "mainloop-api-token" && m.ReadOnly && filepath.Join(m.MountPath, src.Path) == file {
			return *src.ExpirationSeconds
		}
	}
	t.Fatalf("container %s: MAINLOOP_API_TOKEN_FILE=%q is not the mounted token (%+v)", c.Name, file, c.VolumeMounts)
	return 0
}

// Every install lets its machine callers authenticate (docs/specs/authentication.md): the backend
// may create TokenReviews and nothing more, the allowlist names this release's own ServiceAccounts,
// and each caller carries a projected token for the API's audience. Kind has no machine callers (see
// TestKindBootsWithoutKagentOrGitHubApp).
func TestMachineCallersAuthenticate(t *testing.T) {
	for _, path := range []string{"defaults", "production", "production-canary", "live-dev"} {
		t.Run(path, func(t *testing.T) {
			r := render(t, path)
			ns := installs[path].namespace
			data := r.configMaps["mainloop-config"].Data
			if data["MAINLOOP_AUTH_MODE"] != "observe" || data["MAINLOOP_AUTH_SA_AUDIENCE"] != "mainloop-api" {
				t.Errorf("auth mode %q, audience %q", data["MAINLOOP_AUTH_MODE"], data["MAINLOOP_AUTH_SA_AUDIENCE"])
			}
			want := []string{ns + "/mainloop-backend=read", ns + "/mainloop-operator=owner"}
			if _, ok := r.cronJobs["mainloop-canary"]; ok {
				want = append(want, ns+"/mainloop-canary=canary")
			}
			if got := strings.Split(data["MAINLOOP_AUTH_SERVICE_ACCOUNTS"], ","); !slices.Equal(got, want) {
				t.Errorf("MAINLOOP_AUTH_SERVICE_ACCOUNTS = %v, want %v", got, want)
			}

			// TokenReview, and only that, for this release's backend.
			var roles []rbacv1.ClusterRole
			for _, c := range r.clusterRoles {
				if strings.Contains(c.Name, "tokenreview") {
					roles = append(roles, c)
				}
			}
			review := []rbacv1.PolicyRule{{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}}}
			if len(roles) != 1 || roles[0].Name != "mainloop-tokenreview-"+ns || !reflect.DeepEqual(roles[0].Rules, review) {
				t.Fatalf("TokenReview ClusterRole: %+v", roles)
			}
			bound := false
			for _, b := range r.bindings {
				if b.RoleRef.Name == roles[0].Name {
					bound = len(b.Subjects) == 1 && b.Subjects[0] == rbacv1.Subject{Kind: "ServiceAccount", Name: "mainloop-backend", Namespace: ns}
				}
			}
			if !bound {
				t.Fatal("the TokenReview role is not bound to this release's mainloop-backend alone")
			}

			// The operator ServiceAccount has no pod and mounts nothing; its tokens are minted by hand.
			op, ok := r.accounts["mainloop-operator"]
			if !ok || op.AutomountServiceAccountToken == nil || *op.AutomountServiceAccountToken {
				t.Errorf("mainloop-operator: %+v", op)
			}

			pod := r.backend(t)
			for _, c := range pod.Containers {
				apiToken(t, pod, c)
			}

			cj, ok := r.cronJobs["mainloop-canary"]
			if !ok {
				return
			}
			cpod := cj.Spec.JobTemplate.Spec.Template.Spec
			if cpod.ServiceAccountName != "mainloop-canary" || cpod.AutomountServiceAccountToken == nil || *cpod.AutomountServiceAccountToken {
				t.Errorf("canary pod runs as %q, automount %v", cpod.ServiceAccountName, cpod.AutomountServiceAccountToken)
			}
			if sa, ok := r.accounts["mainloop-canary"]; !ok || sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
				t.Errorf("mainloop-canary ServiceAccount: %+v", sa)
			}
			if exp := apiToken(t, cpod, cpod.Containers[0]); exp != 600 {
				t.Errorf("canary token lives %ds, want 600", exp)
			}
		})
	}
	t.Run("off renders none of it", func(t *testing.T) {
		helm, err := exec.LookPath("helm")
		if err != nil {
			t.Skip("helm is not installed")
		}
		out, err := exec.CommandContext(t.Context(), helm, "template", "mainloop", chart, "--namespace", "mainloop",
			"--set", "canary.enabled=true", "--set", "auth.serviceAccounts.enabled=false").Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{"tokenreview", "mainloop-operator", "MAINLOOP_AUTH_SERVICE_ACCOUNTS"} {
			if bytes.Contains(out, []byte(s)) {
				t.Errorf("rendered %q with auth.serviceAccounts off", s)
			}
		}
	})
}

// The screenshot browser (docs/specs/workspaces.md, "Preview screenshots") speaks an
// unauthenticated protocol, so it is off by default and, when on, only the backend reaches it and
// it reaches only DNS and the configured preview gateway, holding no Kubernetes credential.
func TestScreenshotBrowserIsFencedIn(t *testing.T) {
	for _, path := range []string{"defaults", "production", "live-dev"} {
		r := render(t, path)
		if _, ok := r.deployments["mainloop-screenshots"]; ok {
			t.Errorf("%s: the screenshot browser is on by default", path)
		}
		if url := r.configMaps["mainloop-config"].Data["MAINLOOP_SCREENSHOT_BROWSER_URL"]; url != "" {
			t.Errorf("%s: MAINLOOP_SCREENSHOT_BROWSER_URL = %q without a browser", path, url)
		}
	}

	r := render(t, "production-screenshots")
	if got := r.configMaps["mainloop-config"].Data["MAINLOOP_SCREENSHOT_BROWSER_URL"]; got != "http://mainloop-screenshots.mainloop.svc.cluster.local:9222" {
		t.Errorf("MAINLOOP_SCREENSHOT_BROWSER_URL = %q", got)
	}
	d, ok := r.deployments["mainloop-screenshots"]
	if !ok {
		t.Fatal("no mainloop-screenshots Deployment")
	}
	pod := d.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the browser mounts a ServiceAccount token")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("the browser may run as root")
	}
	c := pod.Containers[0]
	if !strings.Contains(c.Image, "@sha256:") {
		t.Errorf("browser image %q is not pinned by digest", c.Image)
	}
	if sc := c.SecurityContext; sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("browser container security context: %+v", c.SecurityContext)
	}
	if len(c.EnvFrom) != 0 {
		t.Errorf("the browser imports env: %+v", c.EnvFrom)
	}
	for _, e := range c.Env {
		if e.ValueFrom != nil {
			t.Errorf("the browser reads %s from %+v", e.Name, e.ValueFrom)
		}
	}

	var policy *networkingv1.NetworkPolicy
	for i, p := range r.policies {
		if p.Spec.PodSelector.MatchLabels["app"] == "mainloop-screenshots" {
			if policy != nil {
				t.Fatalf("two policies select the browser: %s, %s", policy.Name, p.Name)
			}
			policy = &r.policies[i]
		}
	}
	if policy == nil {
		t.Fatal("no NetworkPolicy selects the browser")
	}
	if !slices.Equal(policy.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}) {
		t.Errorf("policy types %v", policy.Spec.PolicyTypes)
	}
	in := policy.Spec.Ingress
	if len(in) != 1 || len(in[0].From) != 1 || in[0].From[0].PodSelector == nil ||
		in[0].From[0].NamespaceSelector != nil || in[0].From[0].PodSelector.MatchLabels["app"] != "mainloop-backend" ||
		len(in[0].Ports) != 1 || in[0].Ports[0].Port.String() != "9222" {
		t.Errorf("browser ingress: %+v", in)
	}
	out := policy.Spec.Egress
	if len(out) != 2 {
		t.Fatalf("browser egress has %d rules: %+v", len(out), out)
	}
	for _, p := range out[0].Ports {
		if p.Port.String() != "53" {
			t.Errorf("DNS rule opens port %s", p.Port)
		}
	}
	gw := out[1]
	if len(gw.Ports) != 1 || gw.Ports[0].Port.String() != "443" || len(gw.To) != 1 || gw.To[0].IPBlock != nil ||
		gw.To[0].PodSelector == nil || gw.To[0].PodSelector.MatchLabels["app"] != "preview-gateway" {
		t.Errorf("browser egress to the gateway: %+v", gw)
	}
}
