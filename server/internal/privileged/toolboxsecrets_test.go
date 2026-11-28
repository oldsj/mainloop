package privileged_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/privileged"
)

const cloudflareToken = "cf-fictional-api-token-9d41c7aa5e"

func secretArgs(script string) map[string]any {
	a := execArgs(script)
	a["secrets"] = []map[string]string{{"ref": "op://kubernetes/cloudflare-api/token", "name": "cf"}}
	a["hosts"] = []string{"api.cloudflare.com"}
	return a
}

func newSecretsEnv(t *testing.T, f *fakeToolbox) *kubeEnv {
	t.Helper()
	e := newToolboxEnv(t, f)
	e.ex.ToolboxSecrets = true
	e.op.Put("kubernetes", "cloudflare-api", "token", cloudflareToken)
	e.op.Put("kubernetes", "mainloop-executor-ssh-nas", "private-key", "fictional-key-material")
	e.op.Put("kubernetes", "op-gate-connect", "token", "fictional-connect-token")
	e.op.Put("Personal", "bank", "password", "fictional-pass")
	return e
}

// Given a secret and the host it may go to, the card shows the item, its version and last edit, the
// field and the host, never the value, with the tag; the agent sees the same. After an Approve
// the run gets the value (once, read at run time) and only the named host; the output has it
// removed exactly, plain and base64.
func TestExecSecretsAndHosts(t *testing.T) {
	f := newFakeToolbox()
	f.finishes(podRan, "calling the API with "+cloudflareToken+"\nencoded "+base64.StdEncoding.EncodeToString([]byte(cloudflareToken))+"\nzone ok\n")
	e := newSecretsEnv(t, f)
	a := e.request(t, privileged.TypeExec, "exec-secret", secretArgs("curl -sS -H \"X-Auth-Key: $(cat /run/secrets/cf)\" https://api.cloudflare.com/client/v4/zones"))
	a, card := e.preflighted(t, a.ID)
	cardHas(t, card, "/run/secrets/cf", `"kubernetes" field "token" of item "cloudflare-api" (version 1`, "api.cloudflare.com", "never an environment variable",
		"only the hosts below", "The values never appear on this card", "uses cloudflare-api/token; can send it to api.cloudflare.com")
	if strings.Contains(card, cloudflareToken) || strings.Contains(string(a.Facts), cloudflareToken) || strings.Contains(string(privileged.AgentFacts(a.Type, a.Facts)), cloudflareToken) {
		t.Fatal("a value is in the card or the facts")
	}
	if e.op.Reads != 0 {
		t.Fatalf("preflight read a value (%d reads)", e.op.Reads)
	}
	e.approve(t, a.ID)
	e.runExecutor(t)
	a = e.get(t, a.ID)
	if a.State != privileged.StateSucceeded || len(f.created) != 1 {
		t.Fatalf("%+v (%v), pods %d", a, deref(a.LastError), len(f.created))
	}
	if got := string(f.created[0].Secrets["cf"]); got != cloudflareToken || len(f.created[0].Secrets) != 1 || len(f.created[0].Hosts) != 1 || f.created[0].Hosts[0] != "api.cloudflare.com" {
		t.Fatalf("spec = %+v", f.created[0])
	}
	res := kubectlResult(t, a)
	if strings.Contains(res.Stdout, cloudflareToken) || strings.Contains(res.Stdout, base64.StdEncoding.EncodeToString([]byte(cloudflareToken))) ||
		!strings.Contains(res.Stdout, "zone ok") || strings.Count(res.Stdout, "[redacted]") != 2 {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	for _, ev := range []string{string(a.Result), deref(a.LastError)} {
		if strings.Contains(ev, cloudflareToken) {
			t.Fatal("a value is stored")
		}
	}
}

// A run takes no secret from another vault, from an item the executor or the 1Password operator or
// op-gate depends on, from an empty or unknown field, and an executor without the feature refuses.
func TestExecSecretsRefusals(t *testing.T) {
	f := newFakeToolbox()
	f.finishes(podRan, "")
	e := newSecretsEnv(t, f)
	for name, tc := range map[string]struct {
		ref, want string
	}{
		"the executor's own key": {"op://kubernetes/mainloop-executor-ssh-nas/private-key", "depends on"},
		"op-gate's token":        {"op://kubernetes/op-gate-connect/token", "depends on"},
		"an unknown item":        {"op://kubernetes/nope/token", "nope"},
		"an unknown field":       {"op://kubernetes/cloudflare-api/nope", "no field"},
	} {
		t.Run(name, func(t *testing.T) {
			args := secretArgs("true")
			args["secrets"] = []map[string]string{{"ref": tc.ref, "name": "s"}}
			a := e.request(t, privileged.TypeExec, "exec-"+strings.ReplaceAll(name, " ", "-"), args)
			e.runExecutor(t)
			if a = e.get(t, a.ID); a.State != "preflight_failed" && !strings.Contains(deref(a.LastError), tc.want) {
				t.Fatalf("%+v (%v)", a, deref(a.LastError))
			}
			if !strings.Contains(deref(e.get(t, a.ID).LastError), tc.want) {
				t.Fatalf("last error = %v, want %q", deref(e.get(t, a.ID).LastError), tc.want)
			}
		})
	}
	if len(f.created) != 0 {
		t.Fatal("a pod was made")
	}
	// An executor without the feature.
	e.ex.ToolboxSecrets = false
	a := e.request(t, privileged.TypeExec, "exec-off", secretArgs("true"))
	e.runExecutor(t)
	if a = e.get(t, a.ID); !strings.Contains(deref(a.LastError), "does not take secrets") {
		t.Fatalf("%+v (%v)", a, deref(a.LastError))
	}
}

// An item edited after the owner saw the card is stale: nothing is made and no value is kept.
func TestExecSecretsStaleItem(t *testing.T) {
	f := newFakeToolbox()
	f.finishes(podRan, "")
	e := newSecretsEnv(t, f)
	a := e.request(t, privileged.TypeExec, "exec-secret", secretArgs("true"))
	a, _ = e.preflighted(t, a.ID)
	e.approve(t, a.ID)
	e.op.Put("kubernetes", "cloudflare-api", "token", "rotated-after-the-card")
	e.runExecutor(t)
	if a = e.get(t, a.ID); a.State != privileged.StateStale || !strings.Contains(deref(a.LastError), "changed since the owner saw it") || len(f.created) != 0 {
		t.Fatalf("%+v (%v), pods %d", a, deref(a.LastError), len(f.created))
	}
}

// A guard added after the owner saw the card also stops the run.
func TestExecSecretsGuardListIsConfigurable(t *testing.T) {
	f := newFakeToolbox()
	f.finishes(podRan, "")
	e := newSecretsEnv(t, f)
	e.ex.ToolboxGuardedItems = []string{"cloudflare-*"}
	a := e.request(t, privileged.TypeExec, "exec-secret", secretArgs("true"))
	e.runExecutor(t)
	if a = e.get(t, a.ID); !strings.Contains(deref(a.LastError), "depends on") {
		t.Fatalf("%+v (%v)", a, deref(a.LastError))
	}
}

// Secrets and hosts are validated: they go together, only on internet reach, with a bounded
// number, file-safe names, op:// references in vault kubernetes, and public fully qualified names.
func TestExecSecretsArguments(t *testing.T) {
	e := newSecretsEnv(t, newFakeToolbox())
	with := func(f func(map[string]any)) map[string]any {
		a := secretArgs("true")
		f(a)
		return a
	}
	one := func(ref, name string) []map[string]string { return []map[string]string{{"ref": ref, "name": name}} }
	many := func(n int) []map[string]string {
		var out []map[string]string
		for i := range n {
			out = append(out, map[string]string{"ref": "op://kubernetes/i" + string(rune('a'+i)) + "/f", "name": "s" + string(rune('a'+i))})
		}
		return out
	}
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"hosts alone":      {with(func(a map[string]any) { delete(a, "secrets") }), "hosts are for a run with secrets"},
		"secrets, no host": {with(func(a map[string]any) { delete(a, "hosts") }), "names the hosts"},
		"cluster reach":    {with(func(a map[string]any) { a["reach"] = "cluster" }), "need reach"},
		"nine secrets":     {with(func(a map[string]any) { a["secrets"] = many(9) }), "at most 8 secrets"},
		"a bad name":       {with(func(a map[string]any) { a["secrets"] = one("op://kubernetes/x/y", "../etc") }), "lower-case letters"},
		"a repeated name": {with(func(a map[string]any) {
			a["secrets"] = []map[string]string{{"ref": "op://kubernetes/x/y", "name": "s"}, {"ref": "op://kubernetes/x/z", "name": "s"}}
		}), "used twice"},
		"a repeated ref": {with(func(a map[string]any) {
			a["secrets"] = []map[string]string{{"ref": "op://kubernetes/x/y", "name": "s"}, {"ref": "op://kubernetes/x/y", "name": "t"}}
		}), "asked for twice"},
		"another vault":   {with(func(a map[string]any) { a["secrets"] = one("op://Personal/x/y", "s") }), "vault \"kubernetes\" only"},
		"not op://":       {with(func(a map[string]any) { a["secrets"] = one("kubernetes/x/y", "s") }), "op://kubernetes"},
		"no field":        {with(func(a map[string]any) { a["secrets"] = one("op://kubernetes/x", "s") }), "op://kubernetes"},
		"a wildcard":      {with(func(a map[string]any) { a["hosts"] = []string{"*.cloudflare.com"} }), "fully qualified"},
		"an address":      {with(func(a map[string]any) { a["hosts"] = []string{"1.1.1.1"} }), "fully qualified"},
		"a port":          {with(func(a map[string]any) { a["hosts"] = []string{"api.cloudflare.com:443"} }), "fully qualified"},
		"upper case":      {with(func(a map[string]any) { a["hosts"] = []string{"API.cloudflare.com"} }), "fully qualified"},
		"a cluster name":  {with(func(a map[string]any) { a["hosts"] = []string{"kubernetes.default.svc"} }), "not a public name"},
		"a home name":     {with(func(a map[string]any) { a["hosts"] = []string{"id.olds.network"} }), "not a public name"},
		"a local name":    {with(func(a map[string]any) { a["hosts"] = []string{"nas.local"} }), "not a public name"},
		"a repeated host": {with(func(a map[string]any) { a["hosts"] = []string{"a.example.org", "a.example.org"} }), "twice"},
		"nine hosts": {with(func(a map[string]any) {
			a["hosts"] = []string{"a.example.org", "b.example.org", "c.example.org", "d.example.org", "e.example.org", "f.example.org", "g.example.org", "h.example.org", "i.example.org"}
		}), "at most 8 hosts"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.svc.Request(context.Background(), mainSession, privileged.TypeExec, jsonArgs(tc.args), "r", "req-"+strings.ReplaceAll(name, " ", "-"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
