package deploytest

import (
	"maps"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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

// With executor.kubernetes, the executor's own account may only mint tokens for two narrow
// accounts; neither runs a pod. The workloads account may patch workloads and nothing holding a
// Secret. The delete account is the only cluster-wide grant over every resource, Secrets included
// (RBAC cannot say "all but"): the executor reads through it with metadata-only requests and
// refuses its own namespace and roles. Both roles are named after the executor's namespace.
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

	for _, name := range []string{"mainloop-executor-workloads", "mainloop-executor-delete"} {
		a, ok := r.accounts[name]
		if !ok || a.Namespace != "mainloop-executor" || a.AutomountServiceAccountToken == nil || *a.AutomountServiceAccountToken {
			t.Errorf("account %s = %+v", name, a)
		}
	}
	for _, d := range r.deployments {
		if sa := d.Spec.Template.Spec.ServiceAccountName; sa == "mainloop-executor-workloads" || sa == "mainloop-executor-delete" {
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
	for name, role := range roles {
		if name == "mainloop-executor-delete" {
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
		!slices.Equal(tokens[0].ResourceNames, []string{"mainloop-executor-delete", "mainloop-executor-workloads"}) {
		t.Errorf("token rules = %+v", tokens)
	}
}
