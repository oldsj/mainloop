package privileged

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// restartRow is a fictional k8s.rollout_restart row as preflight stores it.
func restartRow(t *testing.T) dbq.PrivilegedAction {
	t.Helper()
	args, err := normalizeRestart(json.RawMessage(`{"namespace":"shop","kind":"Deployment","name":"web"}`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := canonical(args)
	if err != nil {
		t.Fatal(err)
	}
	f, err := canonical(WorkloadFacts{
		Workload:  Workload{Kind: "Deployment", Namespace: "shop", Name: "web"},
		CheckedAt: time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return dbq.PrivilegedAction{ID: "pa-0001", Type: TypeRolloutRestart, Version: 1, Args: a, FrozenFacts: f}
}

// Given a stored action, the executor runs it only when its arguments and facts are exactly its
// type's encoding: a key in another letter case, a duplicate key, an unknown key or trailing data
// could show the owner one value and run another, and is refused.
func TestExactRow(t *testing.T) {
	if err := exactRow(restartRow(t)); err != nil {
		t.Fatalf("a row as preflight stores it: %v", err)
	}
	facts := string(restartRow(t).FrozenFacts)
	cases := []struct {
		name, args, facts, want string
	}{
		{"an argument key in another case", `{"Namespace":"shop","kind":"Deployment","name":"web"}`, "", "round-trip"},
		{"the same key in two cases", `{"namespace":"shop","Namespace":"payments","kind":"Deployment","name":"web"}`, "", "round-trip"},
		{"an unknown argument", `{"namespace":"shop","kind":"Deployment","name":"web","note":"x"}`, "", "unknown field"},
		{"arguments that no longer validate", `{"namespace":"Shop!","kind":"Deployment","name":"web"}`, "", "no longer validate"},
		{"a fact key in another case", "", strings.Replace(facts, `"workload"`, `"Workload"`, 1), "round-trip"},
		{"an unknown fact", "", strings.Replace(facts, `{"checked_at"`, `{"extra":1,"checked_at"`, 1), "unknown field"},
		{"data after the facts", "", facts + ` {}`, "data after"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := restartRow(t)
			if c.args != "" {
				row.Args = json.RawMessage(c.args)
			}
			if c.facts != "" {
				row.FrozenFacts = json.RawMessage(c.facts)
			}
			if err := exactRow(row); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// Every type names its facts' type, which exactRow decodes into.
func TestEveryTypeNamesItsFacts(t *testing.T) {
	for name, e := range catalog {
		if e.facts == nil {
			t.Errorf("%s has no facts type", name)
		}
	}
}
