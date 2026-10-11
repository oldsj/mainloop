package mcpserver_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
)

// The tools no slice has landed yet. Each slice deletes its names from here when it registers
// them, which is what makes the missing set exact: a tool missing for any other reason fails.
var pendingSlices = map[string]string{
	"delegate": "G8a", "report": "G8a", "task_get": "G8a", "task_list": "G8a",
	"task_history": "G8a", "task_cancel": "G8a",
	"open_pull_request": "G10", "prepare_pull_request_merge": "G10", "merge_pull_request": "G10",
	"merge_pull_request_with_approval": "G10", "get_pull_request_merge_status": "G10",
}

type contract struct {
	Tools []struct {
		Name        string         `json:"name"`
		InputSchema map[string]any `json:"input_schema"`
	} `json:"tools"`
	Visibility []struct {
		Role            *string  `json:"role"`
		Depth           *int     `json:"depth"`
		Grant           *string  `json:"grant"`
		MergeStatusOnly bool     `json:"merge_status_only"`
		MergeEnabled    bool     `json:"merge_tools_enabled"`
		Ordinary        []string `json:"ordinary"`
		Approval        []string `json:"approval"`
	} `json:"visibility"`
}

func loadContract(t *testing.T) contract {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "mcp-tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c contract
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestToolSetMatchesTheContract(t *testing.T) {
	c := loadContract(t)
	var contractNames []string
	for _, tool := range c.Tools {
		contractNames = append(contractNames, tool.Name)
	}
	slices.Sort(contractNames)
	policyNames := slices.Clone(agentauth.AllTools)
	slices.Sort(policyNames)
	if !slices.Equal(contractNames, policyNames) {
		t.Fatalf("policy table tools %v differ from the contract's %v", policyNames, contractNames)
	}

	implemented := map[string]bool{}
	for _, tool := range (&mcpserver.Server{}).Implemented() {
		implemented[tool.Name] = true
	}
	var missing []string
	for _, name := range contractNames {
		if !implemented[name] {
			missing = append(missing, name)
		}
	}
	var allowed []string
	for name := range pendingSlices {
		allowed = append(allowed, name)
	}
	slices.Sort(missing)
	slices.Sort(allowed)
	if !slices.Equal(missing, allowed) {
		t.Errorf("tools missing from the server: %v\nthe slices that own the missing tools: %v", missing, allowed)
	}
	for name := range implemented {
		if !slices.Contains(contractNames, name) {
			t.Errorf("tool %q is not in the contract", name)
		}
	}
}

func TestVisibilityMatchesTheContract(t *testing.T) {
	c := loadContract(t)
	type key struct {
		role, grant string
		depth       int
		statusOnly  bool
		merge       bool
	}
	want := map[key][2][]string{}
	for _, v := range c.Visibility {
		k := key{statusOnly: v.MergeStatusOnly, merge: v.MergeEnabled}
		if !v.MergeStatusOnly {
			k.role, k.depth, k.grant = *v.Role, *v.Depth, *v.Grant
		}
		want[k] = [2][]string{v.Ordinary, v.Approval}
	}
	sorted := func(s []string) []string {
		s = slices.Clone(s)
		slices.Sort(s)
		if s == nil {
			s = []string{}
		}
		return s
	}
	check := func(k key, a agentauth.Actor) {
		p := agentauth.Policy{MergeTools: k.merge}
		w := want[k] // absent means the actor sees nothing
		for i, surface := range []agentauth.Surface{agentauth.SurfaceOrdinary, agentauth.SurfaceApproval} {
			got, exp := sorted(p.Tools(a, surface)), sorted(w[i])
			if !slices.Equal(got, exp) {
				t.Errorf("%+v on %s:\n got  %v\n want %v", k, surface, got, exp)
			}
		}
	}
	grants := []dbstate.MCPGrantKind{dbstate.GrantNone, dbstate.GrantCoordination, dbstate.GrantWorkspace}
	roles := []dbstate.BindingRole{dbstate.RoleMain, dbstate.RoleSupervisor, dbstate.RoleChild, dbstate.RoleAgent}
	seen := 0
	for _, merge := range []bool{false, true} {
		check(key{statusOnly: true, merge: merge}, agentauth.Actor{MergeStatusOnly: true})
		for _, role := range roles {
			for depth := 0; depth <= 2; depth++ {
				for _, grant := range grants {
					k := key{role: string(role), depth: depth, grant: string(grant), merge: merge}
					check(k, agentauth.Actor{Role: role, Depth: depth, Grant: grant})
					seen++
				}
			}
		}
	}
	if seen != 2*4*3*3 {
		t.Fatal(fmt.Sprint("walked ", seen, " actors"))
	}
}

// The properties, required fields and string lengths of every implemented tool's input match the
// Python models. Descriptions and wording of the schema may differ.
func TestInputSchemasMatchTheContract(t *testing.T) {
	c := loadContract(t)
	contractSchema := map[string]map[string]any{}
	for _, tool := range c.Tools {
		contractSchema[tool.Name] = tool.InputSchema
	}
	for _, tool := range (&mcpserver.Server{}).Implemented() {
		want, got := contractSchema[tool.Name], tool.InputSchema
		if got["additionalProperties"] != false || want["additionalProperties"] != false {
			t.Errorf("%s: additionalProperties differs", tool.Name)
		}
		if fmt.Sprint(required(got)) != fmt.Sprint(required(want)) {
			t.Errorf("%s: required %v, contract %v", tool.Name, required(got), required(want))
		}
		gotProps, wantProps := props(got), props(want)
		if len(gotProps) != len(wantProps) {
			t.Errorf("%s: properties %v, contract %v", tool.Name, keys(gotProps), keys(wantProps))
		}
		for name, wp := range wantProps {
			gp, ok := gotProps[name]
			if !ok {
				t.Errorf("%s: missing property %q", tool.Name, name)
				continue
			}
			if g, w := lengths(gp), lengths(wp); g != w {
				t.Errorf("%s.%s: lengths %v, contract %v", tool.Name, name, g, w)
			}
		}
	}
}

func required(schema map[string]any) []string {
	var out []string
	switch r := schema["required"].(type) {
	case []any:
		for _, v := range r {
			out = append(out, v.(string))
		}
	case []string:
		out = r
	}
	slices.Sort(out)
	return out
}

func props(schema map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for name, v := range schema["properties"].(map[string]any) {
		out[name] = v.(map[string]any)
	}
	return out
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// lengths reads minLength and maxLength from a property or from the non-null branch of its
// optional form.
func lengths(prop map[string]any) [2]int {
	if branches, ok := prop["anyOf"].([]any); ok {
		for _, b := range branches {
			if m := b.(map[string]any); m["type"] != "null" {
				prop = m
			}
		}
	}
	num := func(k string) int {
		switch v := prop[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
		return 0
	}
	return [2]int{num("minLength"), num("maxLength")}
}
