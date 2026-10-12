// Package deploytest renders the tracked Kubernetes manifests offline and checks the properties
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
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const manifests = "../../../k8s/apps/mainloop"

// rendered is one kustomization after `kubectl kustomize`, split by kind.
type rendered struct {
	deployments  map[string]appsv1.Deployment
	statefulSets map[string]appsv1.StatefulSet
	configMaps   map[string]corev1.ConfigMap
	services     map[string]corev1.Service
	policies     []networkingv1.NetworkPolicy
	clusterRoles []rbacv1.ClusterRole
	bindings     []rbacv1.ClusterRoleBinding
	roleBindings []rbacv1.RoleBinding
	// objects is "Kind/name" for every object, so a test can assert what is absent.
	objects []string
}

// render builds a kustomization with the local kubectl (client-side, offline).
func render(t *testing.T, path string) rendered {
	t.Helper()
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl is not installed")
	}
	cmd := exec.CommandContext(t.Context(), kubectl, "kustomize", filepath.Join(manifests, path))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl kustomize %s: %v\n%s", path, err, stderr.String())
	}

	r := rendered{
		deployments:  map[string]appsv1.Deployment{},
		statefulSets: map[string]appsv1.StatefulSet{},
		configMaps:   map[string]corev1.ConfigMap{},
		services:     map[string]corev1.Service{},
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

var overlays = []string{"base", "overlays/prod", "overlays/dev-live", "overlays/kind"}

func TestServerContainersRunTheGoBinary(t *testing.T) {
	for _, path := range overlays {
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
		"backend": {"serve-api", []int32{8000}},
		"mcp":     {"serve-mcp", []int32{8002}},
		"git":     {"serve-git", []int32{8003, 8004}},
	}
	for _, path := range overlays {
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
	for _, path := range overlays {
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

func TestMigrateInitContainerRunsFirst(t *testing.T) {
	for _, path := range overlays {
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
	for _, path := range overlays {
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

// Base and prod require the GitHub App keys from mainloop-secrets; dev-live makes them optional.
func TestGitHubAppKeysComeFromMainloopSecrets(t *testing.T) {
	for _, path := range overlays {
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
					if optional := ref.SecretKeyRef.Optional != nil && *ref.SecretKeyRef.Optional; optional != (path == "overlays/dev-live") {
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
	for _, path := range overlays {
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
	for _, path := range overlays {
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
	for _, path := range overlays {
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
	flags := []string{"GIT_TRANSPORT_ENABLED", "PUSH_GATE_ENABLED", "MAINLOOP_MERGE_TOOLS_ENABLED", "MAINLOOP_OWNER_HITL_WRITES_ENABLED", "MAINLOOP_OWNER_POLICY_WRITES_ENABLED"}
	// The live dev instance is a single-owner prototype, so the owner turned HITL answers on there.
	// Every other overlay, prod included, keeps every flag off.
	allowed := map[string]string{"overlays/dev-live": "MAINLOOP_OWNER_HITL_WRITES_ENABLED"}
	for _, path := range overlays {
		t.Run(path, func(t *testing.T) {
			data := render(t, path).configMaps["mainloop-config"].Data
			for _, flag := range flags {
				if allowed[path] == flag {
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
	data := render(t, "overlays/prod").configMaps["mainloop-config"].Data
	if data["MAINLOOP_ALLOW_RESET"] != "" {
		t.Error("prod must not allow `migrate reset`")
	}
	for _, c := range render(t, "overlays/prod").backend(t).Containers {
		if strings.Contains(c.Image, "-dev") {
			t.Errorf("prod container %s runs dev image %s", c.Name, c.Image)
		}
	}
}

func TestLiveDev(t *testing.T) {
	r := render(t, "overlays/dev-live")
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
		if env["__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS"].Value != "mainloop-dev.olds.network" {
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
			if env["KAGENT_USER_ID"].Value != "mainloop" || env["KAGENT_CONTROL_TOKEN_FILE"].Value != "/run/kagent-control-auth/current" ||
				env["SSL_CERT_FILE"].Value != "/run/kagent-control-ca/ca-bundle.crt" {
				t.Errorf("%s: control-auth env missing: %+v", name, env)
			}
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

// The backend publishes per-session MCP credential Secrets into kagent, so its ServiceAccount
// needs the production Role there. `make dev-agents` applies the binding.
func TestLiveDevBackendMayPublishAgentCredentials(t *testing.T) {
	live := render(t, "overlays/dev-live")
	if got := live.backend(t).ServiceAccountName; got != "mainloop-backend" {
		t.Fatalf("backend ServiceAccount %q, want mainloop-backend", got)
	}
	var binding *rbacv1.RoleBinding
	for _, b := range render(t, "overlays/dev-live/kagent").roleBindings {
		if b.Name == "mainloop-dev-agent-tokens" {
			binding = &b
		}
	}
	if binding == nil {
		t.Fatal("no RoleBinding mainloop-dev-agent-tokens in the dev-live kagent render")
	}
	if binding.Namespace != "kagent" {
		t.Errorf("namespace %q, want kagent", binding.Namespace)
	}
	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != "mainloop-agent-tokens" {
		t.Errorf("roleRef %+v, want Role mainloop-agent-tokens", binding.RoleRef)
	}
	want := rbacv1.Subject{Kind: "ServiceAccount", Name: "mainloop-backend", Namespace: "mainloop-dev"}
	if len(binding.Subjects) != 1 || binding.Subjects[0] != want {
		t.Errorf("subjects %+v, want only %+v", binding.Subjects, want)
	}
}

// The Kind overlay (`make dev`) runs the same Go images as prod against a local PostgreSQL. It must
// boot without kagent, the GitHub App or a registry pull, and its Services and origin must match
// the port forwards in devspace.yaml and the Makefile.
func TestKindOverlayBootsWithoutKagentOrGitHubApp(t *testing.T) {
	r := render(t, "overlays/kind")

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
