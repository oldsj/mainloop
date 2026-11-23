package deploytest

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
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
}
