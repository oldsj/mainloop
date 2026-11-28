package deploytest

import (
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/oldsj/mainloop/server/internal/approval"
)

// serve-executor is off by default. On, it is one replica replaced rather than rolled, in its own
// namespace, with no Service, every ingress denied, its own database login and 1Password token,
// and Secret access only in the namespaces listed; serve-api is told an executor runs.
func TestExecutor(t *testing.T) {
	for _, path := range installNames {
		r := render(t, path)
		if _, ok := r.deployments["mainloop-executor"]; ok {
			t.Errorf("%s renders the executor", path)
		}
		if v := r.configMaps["mainloop-config"].Data["MAINLOOP_PRIVILEGED_EXECUTOR_ENABLED"]; v != "false" {
			t.Errorf("%s: MAINLOOP_PRIVILEGED_EXECUTOR_ENABLED=%q", path, v)
		}
	}

	r := render(t, "production-executor")
	d, ok := r.deployments["mainloop-executor"]
	if !ok {
		t.Fatal("no executor Deployment")
	}
	if d.Namespace != "mainloop-executor" || *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("deployment = %s, %d replicas, %s", d.Namespace, *d.Spec.Replicas, d.Spec.Strategy.Type)
	}
	pod := d.Spec.Template.Spec
	c := pod.Containers[0]
	if !slices.Equal(c.Args, []string{"serve-executor"}) || strings.Contains(c.Image, "-dev") || len(c.Ports) != 0 {
		t.Errorf("container = %v %s %v", c.Args, c.Image, c.Ports)
	}
	if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("the root filesystem is writable")
	}
	env := map[string]string{}
	for _, e := range c.Env {
		v := e.Value
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			v = "secret:" + e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key
		}
		env[e.Name] = v
	}
	for k, want := range map[string]string{
		"DB_USER": "secret:mainloop-executor-db/username", "DB_PASSWORD": "secret:mainloop-executor-db/password",
		"MAINLOOP_EXECUTOR_OP_TOKEN": "secret:mainloop-executor-1password/token", "MAINLOOP_EXECUTOR_SECRET_NAMESPACES": "widgets,billing",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if len(c.EnvFrom) != 0 {
		t.Error("the executor imports an env source; it must name each value")
	}
	for name, s := range r.services {
		if s.Namespace == "mainloop-executor" {
			t.Errorf("Service %s in the executor's namespace", name)
		}
	}
	denied := false
	for _, p := range r.policies {
		if p.Namespace == "mainloop-executor" && len(p.Spec.PodSelector.MatchLabels) == 0 && len(p.Spec.Ingress) == 0 &&
			slices.Contains(p.Spec.PolicyTypes, "Ingress") {
			denied = true
		}
	}
	if !denied {
		t.Error("no policy denies all ingress to the executor's namespace")
	}
	var bound []string
	for _, b := range r.roleBindings {
		if b.Subjects[0].Name == "mainloop-executor" {
			bound = append(bound, b.Namespace)
		}
	}
	if slices.Sort(bound); !slices.Equal(bound, []string{"billing", "widgets"}) {
		t.Errorf("Secret access in %v", bound)
	}
	if v := r.configMaps["mainloop-config"].Data["MAINLOOP_PRIVILEGED_EXECUTOR_ENABLED"]; v != "true" {
		t.Errorf("serve-api's flag = %q", v)
	}
	if v := r.configMaps["mainloop-config"].Data["MAINLOOP_PRIVILEGED_EXECUTOR_KUBERNETES_ENABLED"]; v != "false" {
		t.Errorf("the k8s.* types are on without executor.kubernetes: %q", v)
	}
	if len(r.clusterRoles) != len(render(t, "production").clusterRoles) || r.accounts["mainloop-executor-delete"].Name != "" {
		t.Error("the executor renders Kubernetes access without executor.kubernetes")
	}
}

// With executor.omni, serve-api accepts the Sidero types and the executor gets Omni's endpoint, the
// clusters talosctl may target, and its Omni key from a Secret; it gets no Kubernetes access.
func TestExecutorOmni(t *testing.T) {
	if v := render(t, "production-executor").configMaps["mainloop-config"].Data["MAINLOOP_PRIVILEGED_EXECUTOR_OMNI_ENABLED"]; v != "false" {
		t.Errorf("the Sidero types are on without executor.omni: %q", v)
	}
	r := render(t, "production-executor-omni")
	if v := r.configMaps["mainloop-config"].Data["MAINLOOP_PRIVILEGED_EXECUTOR_OMNI_ENABLED"]; v != "true" {
		t.Errorf("serve-api's flag = %q", v)
	}
	env := envByName(r.deployments["mainloop-executor"].Spec.Template.Spec.Containers[0])
	key := env["MAINLOOP_EXECUTOR_OMNI_KEY"].ValueFrom
	if env["MAINLOOP_EXECUTOR_OMNI_ENABLED"].Value != "true" || env["MAINLOOP_EXECUTOR_OMNI_ENDPOINT"].Value != "https://omni.example.com" ||
		env["MAINLOOP_EXECUTOR_OMNI_CLUSTERS"].Value != "internal-01,lab-02" || env["MAINLOOP_EXECUTOR_OMNI_KEY"].Value != "" ||
		key == nil || key.SecretKeyRef == nil || key.SecretKeyRef.Name != "mainloop-executor-omni" || key.SecretKeyRef.Key != "service-account-key" {
		t.Errorf("executor env = %+v", env)
	}
	if _, ok := env["MAINLOOP_EXECUTOR_KUBERNETES_ENABLED"]; ok || r.accounts["mainloop-executor-admin"].Name != "" {
		t.Error("executor.omni renders Kubernetes access")
	}

	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	out, err := exec.CommandContext(t.Context(), helm, "template", "mainloop", chart, "--namespace", "mainloop",
		"--values", filepath.Join(chart, "ci/production-values.yaml"), "--set", "executor.enabled=true", "--set", "executor.approvals.mode=web",
		"--set", "executor.namespace=mainloop-executor", "--set", "executor.omni.enabled=true",
		"--set", "executor.omni.endpoint=http://omni.example.com").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "executor.omni.endpoint must be Omni's https URL") {
		t.Errorf("an http endpoint renders: %v\n%s", err, out)
	}
}

// With executor.ssh, serve-api accepts command.ssh, and the executor gets the host list and each
// host's credential Secret, mounted read-only under its own directory; nothing else.
func TestExecutorSSH(t *testing.T) {
	if v := render(t, "production-executor").configMaps["mainloop-config"].Data["MAINLOOP_PRIVILEGED_EXECUTOR_SSH_ENABLED"]; v != "false" {
		t.Errorf("command.ssh is on without executor.ssh: %q", v)
	}
	r := render(t, "production-executor-ssh")
	if v := r.configMaps["mainloop-config"].Data["MAINLOOP_PRIVILEGED_EXECUTOR_SSH_ENABLED"]; v != "true" {
		t.Errorf("serve-api's flag = %q", v)
	}
	pod := r.deployments["mainloop-executor"].Spec.Template.Spec
	c := pod.Containers[0]
	env := envByName(c)
	if env["MAINLOOP_EXECUTOR_SSH_ENABLED"].Value != "true" || env["MAINLOOP_EXECUTOR_SSH_HOSTS"].Value != "nas-01=192.0.2.20:22,router=192.0.2.1:2222" ||
		env["MAINLOOP_EXECUTOR_SSH_DIR"].Value != "/etc/mainloop-executor/ssh" {
		t.Errorf("executor env = %+v", env)
	}
	for _, host := range []string{"nas-01", "router"} {
		mounted := slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool {
			return m.Name == "ssh-"+host && m.MountPath == "/etc/mainloop-executor/ssh/"+host && m.ReadOnly
		})
		i := slices.IndexFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == "ssh-"+host })
		if !mounted || i < 0 || pod.Volumes[i].Secret == nil || pod.Volumes[i].Secret.SecretName != "mainloop-executor-ssh-"+host ||
			*pod.Volumes[i].Secret.DefaultMode != 0o440 || len(pod.Volumes[i].Secret.Items) != 3 {
			t.Errorf("%s: mounts %+v, volumes %+v", host, c.VolumeMounts, pod.Volumes)
		}
	}
	if _, ok := env["MAINLOOP_EXECUTOR_OMNI_ENABLED"]; ok || r.accounts["mainloop-executor-admin"].Name != "" {
		t.Error("executor.ssh renders Omni or Kubernetes access")
	}

	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	for set, want := range map[string]string{
		"executor.ssh.hosts=null": "executor.ssh.hosts must name a host",
		"executor.ssh.hosts[0].name=NAS,executor.ssh.hosts[0].address=192.0.2.20:22": `"NAS" is not a DNS label`,
		"executor.ssh.hosts[0].name=nas":                                             "nas has no address",
	} {
		out, err := exec.CommandContext(t.Context(), helm, "template", "mainloop", chart, "--namespace", "mainloop",
			"--values", filepath.Join(chart, "ci/production-values.yaml"), "--set", "executor.enabled=true", "--set", "executor.approvals.mode=web",
			"--set", "executor.namespace=mainloop-executor", "--set", "executor.ssh.enabled=true", "--set", set).CombinedOutput()
		if err == nil || !strings.Contains(string(out), want) {
			t.Errorf("%s renders: %v\n%s", set, err, out)
		}
	}
}

// With executor.kubernetes, the executor's own account may only mint tokens for three narrow
// accounts; none runs a pod. The workloads account may patch workloads and nothing holding a
// Secret. The delete and writer accounts are the only cluster-wide grants over every resource,
// Secrets included (RBAC cannot say "all but"): the executor reads through delete with
// metadata-only requests, refuses Secrets through writer, and refuses its own namespace and roles.
// The roles are named after the executor's namespace. command.kubectl's admin account is bound to
// cluster-admin, and an admission policy keeps every one of these accounts, but not the executor's
// own, off the executor's namespace and its cluster-wide objects.
func TestExecutorKubernetes(t *testing.T) {
	r := render(t, "production-executor-kubernetes")
	cfg := r.configMaps["mainloop-config"].Data
	if cfg["MAINLOOP_PRIVILEGED_EXECUTOR_KUBERNETES_ENABLED"] != "true" {
		t.Errorf("serve-api's flag = %q", cfg["MAINLOOP_PRIVILEGED_EXECUTOR_KUBERNETES_ENABLED"])
	}
	c := r.deployments["mainloop-executor"].Spec.Template.Spec.Containers[0]
	env := envByName(c)
	if env["MAINLOOP_EXECUTOR_KUBERNETES_ENABLED"].Value != "true" || env["MAINLOOP_EXECUTOR_CLUSTER"].Value != "internal-01" ||
		env["MAINLOOP_EXECUTOR_NAMESPACE"].ValueFrom == nil || env["MAINLOOP_EXECUTOR_NAMESPACE"].ValueFrom.FieldRef.FieldPath != "metadata.namespace" {
		t.Errorf("executor env = %+v", c.Env)
	}

	for _, name := range []string{"mainloop-executor-workloads", "mainloop-executor-delete", "mainloop-executor-writer", "mainloop-executor-argo", "mainloop-executor-admin"} {
		a, ok := r.accounts[name]
		if !ok || a.Namespace != "mainloop-executor" || a.AutomountServiceAccountToken == nil || *a.AutomountServiceAccountToken {
			t.Errorf("account %s = %+v", name, a)
		}
	}
	for _, d := range r.deployments {
		if sa := d.Spec.Template.Spec.ServiceAccountName; strings.HasPrefix(sa, "mainloop-executor-") {
			t.Errorf("Deployment %s runs as %s", d.Name, sa)
		}
	}

	roles := map[string]rbacv1.ClusterRole{}
	for _, role := range r.clusterRoles {
		roles[role.Name] = role
	}
	bound := map[string]string{}
	for _, b := range r.bindings {
		for _, s := range b.Subjects {
			if s.Namespace == "mainloop-executor" {
				if s.Name == "mainloop-executor" {
					t.Errorf("the executor's own account is bound cluster-wide by %s", b.Name)
				}
				bound[s.Name] = b.RoleRef.Name
			}
		}
	}
	if !maps.Equal(bound, map[string]string{
		"mainloop-executor-workloads": "mainloop-executor-workloads", "mainloop-executor-delete": "mainloop-executor-delete",
		"mainloop-executor-writer": "mainloop-executor-writer", "mainloop-executor-argo": "mainloop-executor-argo",
		"mainloop-executor-admin": "cluster-admin",
	}) {
		t.Errorf("bindings = %v", bound)
	}
	everything := func(rule rbacv1.PolicyRule) bool {
		return slices.Contains(rule.Resources, "*") || slices.Contains(rule.Resources, "secrets") || slices.Contains(rule.Verbs, "*")
	}
	for _, rule := range roles["mainloop-executor-workloads"].Rules {
		if everything(rule) || slices.ContainsFunc(rule.Verbs, func(v string) bool { return v != "get" && v != "patch" && v != "list" }) {
			t.Errorf("workloads rule = %+v", rule)
		}
	}
	del := roles["mainloop-executor-delete"].Rules
	if len(del) != 1 || !slices.Equal(del[0].Verbs, []string{"get", "delete"}) {
		t.Errorf("delete rules = %+v", del)
	}
	writer := roles["mainloop-executor-writer"].Rules
	if len(writer) != 2 || !slices.Equal(writer[0].Verbs, []string{"get", "create", "patch"}) ||
		!slices.Equal(writer[1].Resources, []string{"roles", "clusterroles"}) || !slices.Equal(writer[1].Verbs, []string{"escalate", "bind"}) {
		t.Errorf("writer rules = %+v", writer)
	}
	argo := roles["mainloop-executor-argo"].Rules
	if len(argo) != 1 || !slices.Equal(argo[0].APIGroups, []string{"argoproj.io"}) || !slices.Equal(argo[0].Resources, []string{"applications"}) ||
		!slices.Equal(argo[0].Verbs, []string{"get", "patch"}) {
		t.Errorf("argo rules = %+v", argo)
	}
	for _, rule := range append(slices.Clone(del), writer...) {
		if slices.Contains(rule.Verbs, "*") || slices.ContainsFunc(rule.Verbs, func(v string) bool { return v == "update" || v == "deletecollection" || v == "impersonate" }) {
			t.Errorf("rule grants more than its type needs: %+v", rule)
		}
	}
	for name, role := range roles {
		if name == "mainloop-executor-delete" || name == "mainloop-executor-writer" {
			continue
		}
		for _, rule := range role.Rules {
			if everything(rule) {
				t.Errorf("ClusterRole %s grants %+v", name, rule)
			}
		}
	}

	var tokens []rbacv1.PolicyRule
	for _, role := range r.roles {
		if role.Namespace == "mainloop-executor" {
			tokens = append(tokens, role.Rules...)
		}
	}
	if len(tokens) != 1 || !slices.Equal(tokens[0].Resources, []string{"serviceaccounts/token"}) || !slices.Equal(tokens[0].Verbs, []string{"create"}) ||
		!slices.Equal(tokens[0].ResourceNames, []string{"mainloop-executor-admin", "mainloop-executor-argo", "mainloop-executor-delete", "mainloop-executor-workloads", "mainloop-executor-writer"}) {
		t.Errorf("token rules = %+v", tokens)
	}

	if len(r.admission) != 1 || len(r.admissionBy) != 1 {
		t.Fatalf("admission policies = %d, bindings = %d", len(r.admission), len(r.admissionBy))
	}
	guard, by := r.admission[0], r.admissionBy[0]
	if guard.Name != "mainloop-executor-guard" || by.Spec.PolicyName != guard.Name || !slices.Equal(by.Spec.ValidationActions, []admissionv1.ValidationAction{admissionv1.Deny}) ||
		by.Spec.MatchResources != nil || by.Spec.ParamRef != nil {
		t.Errorf("guard = %s, binding = %+v", guard.Name, by.Spec)
	}
	if fp := guard.Spec.FailurePolicy; fp == nil || *fp != admissionv1.Fail {
		t.Errorf("failure policy = %v", fp)
	}
	rules := guard.Spec.MatchConstraints.ResourceRules
	if len(rules) != 1 || !slices.Equal(rules[0].Operations, []admissionv1.OperationType{"*"}) || !slices.Equal(rules[0].Resources, []string{"*/*"}) ||
		!slices.Equal(rules[0].APIGroups, []string{"*"}) {
		t.Errorf("match = %+v", rules)
	}
	if mc := guard.Spec.MatchConditions; len(mc) != 1 || mc[0].Expression != "request.userInfo.username.startsWith('system:serviceaccount:mainloop-executor:mainloop-executor-')" {
		t.Errorf("match conditions = %+v", mc)
	}
	var checks []string
	for _, v := range guard.Spec.Validations {
		checks = append(checks, v.Expression)
	}
	if !slices.Equal(checks, []string{
		"request.namespace != 'mainloop-executor'",
		"!(request.resource.group == '' && request.resource.resource == 'namespaces' && request.name == 'mainloop-executor')",
		"!(request.resource.group in ['rbac.authorization.k8s.io', 'admissionregistration.k8s.io'] && request.name.startsWith('mainloop-executor-'))",
	}) {
		t.Errorf("validations = %q", checks)
	}
}

// The chart refuses to let onepassword.item_to_secret write Secrets in the executor's own namespace
// or the release's: the executor's database and 1Password credentials, and serve-api's, live there.
func TestExecutorSecretNamespacesRefuseItsOwn(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	for _, ns := range []string{"mainloop-executor", "mainloop"} {
		cmd := exec.CommandContext(t.Context(), helm, "template", "mainloop", chart, "--namespace", "mainloop",
			"--values", filepath.Join(chart, "ci/production-values.yaml"), "--set", "executor.enabled=true", "--set", "executor.approvals.mode=web",
			"--set", "executor.namespace=mainloop-executor", "--set", "executor.secretNamespaces={widgets,"+ns+"}")
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "executor.secretNamespaces must not hold the executor's or the release's namespace ("+ns+")") {
			t.Errorf("%s: err = %v\n%s", ns, err, out)
		}
	}
}

// Given approval keys pinned in the chart, serve-api (through the shared config) and
// serve-executor get the same value, which the binary parses, and the signed mode; in web mode,
// neither has keys.
func TestPinnedApprovalKeysReachBothProcesses(t *testing.T) {
	const mode = "MAINLOOP_PRIVILEGED_APPROVALS_MODE"
	r := render(t, "production-executor-approvals")
	api := r.configMaps["mainloop-config"].Data["MAINLOOP_PRIVILEGED_APPROVAL_KEYS"]
	ex := envByName(r.deployments["mainloop-executor"].Spec.Template.Spec.Containers[0])["MAINLOOP_PRIVILEGED_APPROVAL_KEYS"].Value
	if api == "" || api != ex {
		t.Fatalf("serve-api %q, serve-executor %q", api, ex)
	}
	pinned, err := approval.ParsePinned(ex)
	if err != nil || len(pinned) != 1 || pinned["dev_0123456789abcdefghijklmnop"] == nil {
		t.Fatalf("ParsePinned = %v, %v", pinned, err)
	}
	if api, ex := r.configMaps["mainloop-config"].Data[mode], envByName(r.deployments["mainloop-executor"].Spec.Template.Spec.Containers[0])[mode].Value; api != "signed" || ex != "signed" {
		t.Errorf("mode: serve-api %q, serve-executor %q", api, ex)
	}
	if _, ok := r.configMaps["mainloop-config"].Data["MAINLOOP_PRIVILEGED_APPROVAL_KEYS_REQUIRED"]; ok {
		t.Error("serve-api still has the removed approval-keys-required flag")
	}

	r = render(t, "production-executor")
	if v, ok := r.configMaps["mainloop-config"].Data["MAINLOOP_PRIVILEGED_APPROVAL_KEYS"]; ok {
		t.Errorf("serve-api has keys %q in web mode", v)
	}
	if v, ok := envByName(r.deployments["mainloop-executor"].Spec.Template.Spec.Containers[0])["MAINLOOP_PRIVILEGED_APPROVAL_KEYS"]; ok {
		t.Errorf("serve-executor has keys %+v in web mode", v)
	}
	if api, ex := r.configMaps["mainloop-config"].Data[mode], envByName(r.deployments["mainloop-executor"].Spec.Template.Spec.Containers[0])[mode].Value; api != "web" || ex != "web" {
		t.Errorf("mode: serve-api %q, serve-executor %q", api, ex)
	}
	// With no executor there is no approval mode to choose.
	if v, ok := render(t, "production").configMaps["mainloop-config"].Data[mode]; ok {
		t.Errorf("serve-api has the approval mode %q without an executor", v)
	}
}

// The approval mode is chosen, never defaulted: with an executor and no mode the chart refuses to
// render, so a values change that drops the keys can't fall back to web approvals silently, and the
// choices that contradict each other fail too.
func TestApprovalModeIsChosen(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	key := []string{
		"--set", "executor.approvalKeys.pinned[0].kid=dev_0123456789abcdefghijklmnop", "--set", "executor.approvalKeys.pinned[0].kty=EC",
		"--set", "executor.approvalKeys.pinned[0].crv=P-256", "--set", "executor.approvalKeys.pinned[0].x=VBY4eEqK2i_pljz6fJ7BYbPqbMOb-ZFPzBXzI1_w1mM",
		"--set", "executor.approvalKeys.pinned[0].y=YNqLInY0XbP0fGgecdIgsmrHXiY9zDnuNAG4ozmCWgQ",
	}
	for name, c := range map[string]struct {
		set  []string
		want string // "" renders
	}{
		"no mode":               {nil, "executor.approvals.mode must be set to web or signed"},
		"no mode, keys pinned":  {key, "executor.approvals.mode must be set to web or signed"},
		"unknown mode":          {[]string{"--set", "executor.approvals.mode=optional"}, "executor.approvals.mode must be set to web or signed"},
		"signed, no key":        {[]string{"--set", "executor.approvals.mode=signed"}, "executor.approvals.mode signed needs a key"},
		"signed, key unpinned":  {[]string{"--set", "executor.approvals.mode=signed", "--set", "executor.approvalKeys.pinned=null"}, "executor.approvals.mode signed needs a key"},
		"web with a key":        {append([]string{"--set", "executor.approvals.mode=web"}, key...), "pinned keys make the executor refuse the web app's approvals"},
		"the removed flag":      {[]string{"--set", "executor.approvals.mode=signed", "--set", "executor.approvalKeys.required=true"}, "executor.approvalKeys.required was replaced by executor.approvals.mode"},
		"the removed flag, off": {[]string{"--set", "executor.approvals.mode=web", "--set", "executor.approvalKeys.required=false"}, "executor.approvalKeys.required was replaced by executor.approvals.mode"},
		"web":                   {[]string{"--set", "executor.approvals.mode=web"}, ""},
		"signed":                {append([]string{"--set", "executor.approvals.mode=signed"}, key...), ""},
	} {
		args := append([]string{"template", "mainloop", chart, "--namespace", "mainloop", "--values", filepath.Join(chart, "ci/production-values.yaml"),
			"--set", "executor.enabled=true", "--set", "executor.namespace=mainloop-executor"}, c.set...)
		out, err := exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: does not render: %v\n%s", name, err, out)
		case c.want != "" && (err == nil || !strings.Contains(string(out), c.want)):
			t.Errorf("%s: renders, or fails otherwise (want %q): %v\n%s", name, c.want, err, out)
		}
	}
	// Without an executor the mode is not needed.
	if out, err := exec.CommandContext(t.Context(), helm, "template", "mainloop", chart, "--namespace", "mainloop",
		"--values", filepath.Join(chart, "ci/production-values.yaml")).CombinedOutput(); err != nil {
		t.Errorf("no executor: %v\n%s", err, out)
	}
}

// With executor.unifi, serve-api accepts http.unifi, and the executor gets the console's address
// and its credential Secret, mounted read-only; nothing else.
func TestExecutorUniFi(t *testing.T) {
	if v := render(t, "production-executor").configMaps["mainloop-config"].Data["MAINLOOP_PRIVILEGED_EXECUTOR_UNIFI_ENABLED"]; v != "false" {
		t.Errorf("http.unifi is on without executor.unifi: %q", v)
	}
	r := render(t, "production-executor-unifi")
	if v := r.configMaps["mainloop-config"].Data["MAINLOOP_PRIVILEGED_EXECUTOR_UNIFI_ENABLED"]; v != "true" {
		t.Errorf("serve-api's flag = %q", v)
	}
	pod := r.deployments["mainloop-executor"].Spec.Template.Spec
	c := pod.Containers[0]
	env := envByName(c)
	if env["MAINLOOP_EXECUTOR_UNIFI_ENABLED"].Value != "true" || env["MAINLOOP_EXECUTOR_UNIFI_ADDRESS"].Value != "192.0.2.2:443" ||
		env["MAINLOOP_EXECUTOR_UNIFI_DIR"].Value != "/etc/mainloop-executor/unifi" {
		t.Errorf("executor env = %+v", env)
	}
	mounted := slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == "unifi" && m.MountPath == "/etc/mainloop-executor/unifi" && m.ReadOnly
	})
	i := slices.IndexFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == "unifi" })
	if !mounted || i < 0 || pod.Volumes[i].Secret == nil || pod.Volumes[i].Secret.SecretName != "mainloop-executor-unifi" ||
		*pod.Volumes[i].Secret.DefaultMode != 0o440 || len(pod.Volumes[i].Secret.Items) != 2 {
		t.Errorf("mounts %+v, volumes %+v", c.VolumeMounts, pod.Volumes)
	}
	if _, ok := env["MAINLOOP_EXECUTOR_SSH_ENABLED"]; ok || r.accounts["mainloop-executor-admin"].Name != "" {
		t.Error("executor.unifi renders ssh or Kubernetes access")
	}

	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	for _, addr := range []string{"", "unifi.example.com:443", "192.0.2.2"} {
		out, err := exec.CommandContext(t.Context(), helm, "template", "mainloop", chart, "--namespace", "mainloop",
			"--values", filepath.Join(chart, "ci/production-values.yaml"), "--set", "executor.enabled=true", "--set", "executor.approvals.mode=web",
			"--set", "executor.namespace=mainloop-executor", "--set", "executor.unifi.enabled=true", "--set-string", "executor.unifi.address="+addr).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "executor.unifi.address must be the console's IP:port") {
			t.Errorf("%q renders: %v\n%s", addr, err, out)
		}
	}
}

// command.exec is off unless executor.toolbox is on. On, serve-api accepts it, the executor gets
// the image (by digest), namespace and pull Secrets, and the toolbox account is made: the
// executor's own account may mint its token and nothing else changes. The account may make, read
// and delete pods and their Secrets in the toolbox namespace only, which is Pod Security
// restricted and denies all traffic by default. Nothing is bound cluster-wide.
func TestExecutorToolbox(t *testing.T) {
	if v := render(t, "production-executor-kubernetes").configMaps["mainloop-config"].Data["MAINLOOP_PRIVILEGED_EXECUTOR_TOOLBOX_ENABLED"]; v != "false" {
		t.Errorf("command.exec is on without executor.toolbox: %q", v)
	}
	for _, o := range render(t, "production-executor-kubernetes").objects {
		if strings.Contains(o, "toolbox") {
			t.Errorf("executor.kubernetes alone renders %s", o)
		}
	}
	r := render(t, "production-executor-toolbox")
	if v := r.configMaps["mainloop-config"].Data["MAINLOOP_PRIVILEGED_EXECUTOR_TOOLBOX_ENABLED"]; v != "true" {
		t.Errorf("serve-api's flag = %q", v)
	}
	env := envByName(r.deployments["mainloop-executor"].Spec.Template.Spec.Containers[0])
	for k, want := range map[string]string{
		"MAINLOOP_EXECUTOR_TOOLBOX_ENABLED": "true", "MAINLOOP_EXECUTOR_TOOLBOX_NAMESPACE": "mainloop-toolbox",
		"MAINLOOP_EXECUTOR_TOOLBOX_PULL_SECRETS": "ghcr-pull",
		"MAINLOOP_EXECUTOR_TOOLBOX_IMAGE":        "ghcr.io/oldsj/mainloop-toolbox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		if env[k].Value != want {
			t.Errorf("%s = %q, want %q", k, env[k].Value, want)
		}
	}
	a, ok := r.accounts["mainloop-executor-toolbox"]
	if !ok || a.Namespace != "mainloop-executor" || a.AutomountServiceAccountToken == nil || *a.AutomountServiceAccountToken {
		t.Errorf("account = %+v", a)
	}
	for _, d := range r.deployments {
		if d.Spec.Template.Spec.ServiceAccountName == "mainloop-executor-toolbox" {
			t.Errorf("Deployment %s runs as the toolbox account", d.Name)
		}
	}
	for _, b := range r.bindings {
		for _, s := range b.Subjects {
			if strings.HasPrefix(s.Name, "mainloop-executor") && s.Namespace == "mainloop-executor" {
				t.Errorf("%s is bound cluster-wide by %s: the toolbox needs no cluster-wide grant", s.Name, b.Name)
			}
		}
	}
	var role *rbacv1.Role
	var tokens *rbacv1.Role
	for i := range r.roles {
		switch r.roles[i].Name {
		case "mainloop-executor-toolbox":
			role = &r.roles[i]
		case "mainloop-executor-toolbox-tokens":
			tokens = &r.roles[i]
		}
	}
	if role == nil || role.Namespace != "mainloop-toolbox" || len(role.Rules) != 3 {
		t.Fatalf("role = %+v", role)
	}
	for _, rule := range role.Rules {
		if slices.Contains(rule.Verbs, "*") || slices.Contains(rule.Verbs, "patch") || slices.Contains(rule.Verbs, "update") || slices.Contains(rule.Verbs, "get") && slices.Contains(rule.Resources, "secrets") {
			t.Errorf("rule = %+v", rule)
		}
	}
	if tokens == nil || !slices.Contains(tokens.Rules[0].ResourceNames, "mainloop-executor-toolbox") || !slices.Equal(tokens.Rules[0].Verbs, []string{"create"}) {
		t.Errorf("token role = %+v", tokens)
	}
	if !slices.ContainsFunc(r.roleBindings, func(b rbacv1.RoleBinding) bool {
		return b.Namespace == "mainloop-toolbox" && b.RoleRef.Name == "mainloop-executor-toolbox" &&
			len(b.Subjects) == 1 && b.Subjects[0].Name == "mainloop-executor-toolbox" && b.Subjects[0].Namespace == "mainloop-executor"
	}) {
		t.Errorf("role bindings = %+v", r.roleBindings)
	}
	denied := slices.ContainsFunc(r.policies, func(p networkingv1.NetworkPolicy) bool {
		return p.Namespace == "mainloop-toolbox" && len(p.Spec.PodSelector.MatchLabels) == 0 && len(p.Spec.Ingress) == 0 && len(p.Spec.Egress) == 0 &&
			slices.Contains(p.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) && slices.Contains(p.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)
	})
	if !denied || !slices.Contains(r.objects, "Namespace/mainloop-toolbox") {
		t.Errorf("deny: %v, objects %v", denied, r.objects)
	}
	if got := r.namespaceLabels["mainloop-toolbox"]["pod-security.kubernetes.io/enforce"]; got != "restricted" {
		t.Errorf("Pod Security = %q", got)
	}

	// A bad image and a shared namespace are refused at render.
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	base := []string{"template", "mainloop", chart, "--namespace", "mainloop", "-f", filepath.Join(chart, "ci/production-values.yaml"),
		"--set", "executor.enabled=true", "--set", "executor.namespace=mainloop-executor", "--set", "executor.approvals.mode=web", "--set", "executor.toolbox.enabled=true",
		"--set", "executor.toolbox.image=ghcr.io/oldsj/mainloop-toolbox@sha256:" + strings.Repeat("a", 64)}
	for name, tc := range map[string]struct {
		set  []string
		want string
	}{
		"a tag":                    {[]string{"executor.toolbox.image=ghcr.io/oldsj/mainloop-toolbox:latest"}, "pinned by digest"},
		"the executor's namespace": {[]string{"executor.toolbox.namespace=mainloop-executor"}, "a namespace of its own"},
	} {
		args := slices.Clone(base)
		for _, s := range tc.set {
			args = append(args, "--set", s)
		}
		out, err := exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
		if err == nil || !strings.Contains(string(out), tc.want) {
			t.Errorf("%s: err %v, output %.300s", name, err, out)
		}
	}
}

// Cluster reach is off unless executor.toolbox.clusterReach is on, and then the executor is told
// the proxy sidecar's image: its own, by digest. It needs Kubernetes access (the token is for the
// admin account) and an image pinned by digest.
func TestExecutorToolboxClusterReach(t *testing.T) {
	if _, ok := envByName(render(t, "production-executor-toolbox").deployments["mainloop-executor"].Spec.Template.Spec.Containers[0])["MAINLOOP_EXECUTOR_TOOLBOX_PROXY_IMAGE"]; ok {
		t.Error("cluster reach is on without executor.toolbox.clusterReach")
	}
	r := render(t, "production-executor-toolbox-cluster")
	env := envByName(r.deployments["mainloop-executor"].Spec.Template.Spec.Containers[0])
	want := "ghcr.io/oldsj/mainloop-backend@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	if env["MAINLOOP_EXECUTOR_TOOLBOX_PROXY_IMAGE"].Value != want || env["MAINLOOP_EXECUTOR_TOOLBOX_ENABLED"].Value != "true" {
		t.Errorf("proxy image = %q", env["MAINLOOP_EXECUTOR_TOOLBOX_PROXY_IMAGE"].Value)
	}
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	base := []string{"template", "mainloop", chart, "--namespace", "mainloop", "-f", filepath.Join(chart, "ci/production-values.yaml"),
		"--set", "executor.enabled=true", "--set", "executor.namespace=mainloop-executor", "--set", "executor.approvals.mode=web",
		"--set", "executor.toolbox.enabled=true", "--set", "executor.toolbox.clusterReach=true",
		"--set", "executor.toolbox.image=ghcr.io/oldsj/mainloop-toolbox@sha256:" + strings.Repeat("a", 64)}
	for name, tc := range map[string]struct {
		set  []string
		want string
	}{
		"no kubernetes": {[]string{"executor.image.repository=ghcr.io/x/y", "executor.image.digest=sha256:" + strings.Repeat("b", 64)}, "clusterReach needs executor.kubernetes.enabled"},
		"no digest":     {[]string{"executor.kubernetes.enabled=true"}, "pinned by digest"},
	} {
		args := slices.Clone(base)
		for _, s := range tc.set {
			args = append(args, "--set", s)
		}
		out, err := exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
		if err == nil || !strings.Contains(string(out), tc.want) {
			t.Errorf("%s: err %v, output %.300s", name, err, out)
		}
	}
}

// Run secrets are off unless executor.toolbox.secrets is on. On, the executor is told so and gets
// the guard list, and the toolbox account may also read its Secrets back and make and delete
// CiliumNetworkPolicies in the toolbox namespace; off, it may do neither.
func TestExecutorToolboxSecrets(t *testing.T) {
	roleRules := func(r rendered) []rbacv1.PolicyRule {
		for _, ro := range r.roles {
			if ro.Name == "mainloop-executor-toolbox" {
				return ro.Rules
			}
		}
		t.Fatal("no toolbox role")
		return nil
	}
	off := render(t, "production-executor-toolbox")
	if _, ok := envByName(off.deployments["mainloop-executor"].Spec.Template.Spec.Containers[0])["MAINLOOP_EXECUTOR_TOOLBOX_SECRETS_ENABLED"]; ok {
		t.Error("secrets are on without executor.toolbox.secrets")
	}
	for _, rule := range roleRules(off) {
		if slices.Contains(rule.APIGroups, "cilium.io") || slices.Contains(rule.Resources, "secrets") && slices.Contains(rule.Verbs, "get") {
			t.Errorf("the toolbox account has run-secret rights without executor.toolbox.secrets: %+v", rule)
		}
	}
	on := render(t, "production-executor-toolbox-secrets")
	env := envByName(on.deployments["mainloop-executor"].Spec.Template.Spec.Containers[0])
	if env["MAINLOOP_EXECUTOR_TOOLBOX_SECRETS_ENABLED"].Value != "true" || env["MAINLOOP_EXECUTOR_TOOLBOX_GUARDED_ITEMS"].Value != "mainloop-executor-*,op-gate*,dns-admin" {
		t.Errorf("env = %+v %+v", env["MAINLOOP_EXECUTOR_TOOLBOX_SECRETS_ENABLED"], env["MAINLOOP_EXECUTOR_TOOLBOX_GUARDED_ITEMS"])
	}
	var policies, secretGet bool
	for _, rule := range roleRules(on) {
		if slices.Contains(rule.APIGroups, "cilium.io") {
			policies = slices.Equal(rule.Resources, []string{"ciliumnetworkpolicies"}) && slices.Equal(rule.Verbs, []string{"create", "delete"})
		}
		if slices.Contains(rule.Resources, "secrets") && slices.Equal(rule.Verbs, []string{"get"}) {
			secretGet = true
		}
		if slices.Contains(rule.Verbs, "*") || slices.Contains(rule.Verbs, "patch") || slices.Contains(rule.Verbs, "update") {
			t.Errorf("rule = %+v", rule)
		}
	}
	if !policies || !secretGet {
		t.Errorf("rules = %+v", roleRules(on))
	}
}
