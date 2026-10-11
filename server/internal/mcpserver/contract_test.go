package mcpserver_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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
	visited := map[key]bool{}
	check := func(k key, a agentauth.Actor) {
		visited[k] = true
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
	// A row of the file for a role, depth or grant this walk does not enumerate would otherwise
	// go unchecked.
	for k := range want {
		if !visited[k] {
			t.Errorf("the contract has a visibility row %+v that this test does not enumerate", k)
		}
	}
}

// Every implemented tool's input schema matches the Python model's: the same properties and
// required set, and for each property the type, nullability, length, pattern, enum, numeric and
// item bounds, recursively. Titles, defaults and descriptions may differ.
func TestInputSchemasMatchTheContract(t *testing.T) {
	c := loadContract(t)
	contractSchema := map[string]map[string]any{}
	for _, tool := range c.Tools {
		contractSchema[tool.Name] = tool.InputSchema
	}
	for _, tool := range (&mcpserver.Server{}).Implemented() {
		got, want := normalizeSchema(t, tool.InputSchema, tool.InputSchema), normalizeSchema(t, contractSchema[tool.Name], contractSchema[tool.Name])
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: input schema differs from the contract\n got  %s\n want %s", tool.Name, mustJSON(got), mustJSON(want))
		}
	}
}

// TestSchemaComparisonSeesADifferentType proves the comparison is not shallow: changing one
// property's type, pattern, enum, bound or nullability must change its normalized form.
func TestSchemaComparisonSeesADifferentType(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"type": "object", "additionalProperties": false, "required": []any{"a"},
			"properties": map[string]any{"a": map[string]any{"type": "string", "minLength": float64(1), "pattern": "^x$", "enum": []any{"x", "y"}}},
		}
	}
	prop := func(m map[string]any) map[string]any { return m["properties"].(map[string]any)["a"].(map[string]any) }
	mutations := map[string]func(map[string]any){
		"type":      func(m map[string]any) { prop(m)["type"] = "integer" },
		"pattern":   func(m map[string]any) { prop(m)["pattern"] = "^y$" },
		"enum":      func(m map[string]any) { prop(m)["enum"] = []any{"x"} },
		"minLength": func(m map[string]any) { prop(m)["minLength"] = float64(2) },
		"maximum":   func(m map[string]any) { prop(m)["maximum"] = float64(3) },
		"nullable": func(m map[string]any) {
			m["properties"].(map[string]any)["a"] = map[string]any{"anyOf": []any{prop(m), map[string]any{"type": "null"}}}
		},
		"required": func(m map[string]any) { m["required"] = []any{} },
	}
	want := normalizeSchema(t, base(), base())
	for name, mutate := range mutations {
		m := base()
		mutate(m)
		if reflect.DeepEqual(normalizeSchema(t, m, m), want) {
			t.Errorf("a changed %s is not noticed", name)
		}
	}
	// What may differ does not matter.
	m := base()
	prop(m)["title"], prop(m)["description"], prop(m)["default"] = "A", "words", nil
	if !reflect.DeepEqual(normalizeSchema(t, m, m), want) {
		t.Error("title, description and default should be ignored")
	}
}

// ignoredKeys may differ between the Pydantic schema and the Go one.
var ignoredKeys = map[string]bool{"title": true, "description": true, "default": true, "$defs": true}

// normalizeSchema reduces a schema to what a client can rely on. A nullable property (anyOf with
// a null branch) becomes the other branch plus "nullable": true; $ref is resolved against root's
// $defs; numbers are compared as numbers.
func normalizeSchema(t *testing.T, node, root map[string]any) map[string]any {
	t.Helper()
	return normalizeNode(t, viaJSON(t, node), viaJSON(t, root))
}

// viaJSON gives the Go schema the types a decoded file has: float64 numbers, []any lists.
func viaJSON(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func normalizeNode(t *testing.T, node, root map[string]any) map[string]any {
	t.Helper()
	if ref, ok := node["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/$defs/")
		defs, _ := root["$defs"].(map[string]any)
		target, ok := defs[name].(map[string]any)
		if !ok || name == ref {
			t.Fatalf("unresolvable $ref %q", ref)
		}
		return normalizeNode(t, target, root)
	}
	if branches, ok := node["anyOf"].([]any); ok {
		var rest []any
		nullable := false
		for _, b := range branches {
			if m, _ := b.(map[string]any); m["type"] == "null" && len(m) == 1 {
				nullable = true
			} else {
				rest = append(rest, b)
			}
		}
		if len(rest) != 1 {
			t.Fatalf("anyOf with %d non-null branches is not supported by this test", len(rest))
		}
		out := normalizeNode(t, rest[0].(map[string]any), root)
		if nullable {
			out["nullable"] = true
		}
		return out
	}
	out := map[string]any{}
	for k, v := range node {
		if ignoredKeys[k] {
			continue
		}
		switch k {
		case "properties":
			props := map[string]any{}
			for name, p := range v.(map[string]any) {
				props[name] = normalizeNode(t, p.(map[string]any), root)
			}
			out[k] = props
		case "items":
			out[k] = normalizeNode(t, v.(map[string]any), root)
		case "required", "enum":
			list := []string{}
			for _, e := range v.([]any) {
				list = append(list, fmt.Sprint(e))
			}
			slices.Sort(list)
			out[k] = list
		default:
			out[k] = v
		}
	}
	if _, ok := out["required"]; !ok && out["type"] == "object" {
		out["required"] = []string{}
	}
	return out
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
