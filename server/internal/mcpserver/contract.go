package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// ContractPath is the generated tool contract, relative to the repository root.
const ContractPath = "api/mcp-tools.json"

// Contract is the published shape of api/mcp-tools.json: every tool with its input schema, and the
// exact tools each kind of actor sees on each surface. It is generated from the tool definitions
// and the policy table, so clients read the same facts the server enforces.
type Contract struct {
	Version    int              `json:"version"`
	Surfaces   ContractSurfaces `json:"surfaces"`
	Tools      []ContractTool   `json:"tools"`
	Visibility []ContractRow    `json:"visibility"`
}

// ContractSurfaces maps each surface to its path.
type ContractSurfaces struct {
	Ordinary string `json:"ordinary"`
	Approval string `json:"approval"`
}

// ContractTool is one tool in the contract.
type ContractTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// ContractRow lists what one kind of actor sees. A row with a nil role, depth and grant is the
// actor of a completed task that may only read merge status.
type ContractRow struct {
	Role            *string  `json:"role"`
	Depth           *int     `json:"depth"`
	Grant           *string  `json:"grant"`
	MergeStatusOnly bool     `json:"merge_status_only"`
	MergeEnabled    bool     `json:"merge_tools_enabled"`
	DevLoopEnabled  bool     `json:"devloop_enabled"`
	Ordinary        []string `json:"ordinary"`
	Approval        []string `json:"approval"`
}

// BuildContract derives the contract from the registered tools and the policy table. It fails if
// the policy names a tool the server does not implement.
func BuildContract() (Contract, error) {
	tools, err := contractTools((&Server{}).Implemented(), agentauth.AllTools)
	if err != nil {
		return Contract{}, err
	}
	c := Contract{
		Version:  1,
		Surfaces: ContractSurfaces{Ordinary: PathOrdinary, Approval: PathApproval},
		Tools:    tools,
	}

	row := func(p agentauth.Policy, a agentauth.Actor) (ordinary, approval []string) {
		sorted := func(s agentauth.Surface) []string {
			names := slices.Clone(p.Tools(a, s))
			slices.Sort(names)
			if names == nil {
				names = []string{}
			}
			return names
		}
		return sorted(agentauth.SurfaceOrdinary), sorted(agentauth.SurfaceApproval)
	}
	// Actors that see nothing are left out: every actor not listed sees nothing.
	add := func(r ContractRow, a agentauth.Actor) {
		r.Ordinary, r.Approval = row(agentauth.Policy{MergeTools: r.MergeEnabled, DevLoop: r.DevLoopEnabled}, a)
		if len(r.Ordinary) > 0 || len(r.Approval) > 0 {
			c.Visibility = append(c.Visibility, r)
		}
	}
	for _, merge := range []bool{false, true} {
		for _, dev := range []bool{false, true} {
			add(ContractRow{MergeStatusOnly: true, MergeEnabled: merge, DevLoopEnabled: dev}, agentauth.Actor{MergeStatusOnly: true})
		}
	}
	roles := []dbstate.BindingRole{dbstate.RoleMain, dbstate.RoleSupervisor, dbstate.RoleChild, dbstate.RoleAgent}
	grants := []dbstate.MCPGrantKind{dbstate.GrantNone, dbstate.GrantCoordination, dbstate.GrantWorkspace}
	for _, role := range roles {
		for depth := 0; depth <= 2; depth++ {
			for _, grant := range grants {
				for _, merge := range []bool{false, true} {
					for _, dev := range []bool{false, true} {
						role, depth, grant := string(role), depth, string(grant)
						add(ContractRow{Role: &role, Depth: &depth, Grant: &grant, MergeEnabled: merge, DevLoopEnabled: dev},
							agentauth.Actor{Role: dbstate.BindingRole(role), Depth: depth, Grant: dbstate.MCPGrantKind(grant)})
					}
				}
			}
		}
	}
	return c, nil
}

// contractTools lists the implemented tools in the policy table's order. It fails if the table names
// a tool that is not implemented, or an implemented tool is missing from the table.
func contractTools(implemented []Tool, names []string) ([]ContractTool, error) {
	byName := map[string]Tool{}
	for _, t := range implemented {
		byName[t.Name] = t
	}
	var out []ContractTool
	for _, name := range names {
		t, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("the policy table names tool %q, which the server does not implement", name)
		}
		delete(byName, name)
		out = append(out, ContractTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	for name := range byName {
		return nil, fmt.Errorf("tool %q is implemented but missing from the policy table", name)
	}
	return out, nil
}

// scalarList matches a multi-line array of scalars in indented JSON.
var scalarList = regexp.MustCompile(`(?m)^( *)(.*)\[\n((?: +(?:"(?:[^"\\]|\\.)*"|-?[0-9.eE+-]+|true|false|null),?\n)+) *\](,?)$`)

const printWidth = 100

// ContractJSON renders the contract the way the repository's formatter lays it out, so a
// generated file is already formatted: two-space indent, and an array of scalars on one line when
// it fits in 100 columns.
func ContractJSON() ([]byte, error) {
	c, err := BuildContract()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	out := scalarList.ReplaceAllFunc(buf.Bytes(), func(m []byte) []byte {
		sub := scalarList.FindSubmatch(m)
		var items []string
		for _, line := range strings.Split(strings.TrimSpace(string(sub[3])), "\n") {
			items = append(items, strings.TrimSuffix(strings.TrimSpace(line), ","))
		}
		joined := string(sub[1]) + string(sub[2]) + "[" + strings.Join(items, ", ") + "]" + string(sub[4])
		if len(joined) > printWidth {
			return m
		}
		return []byte(joined)
	})
	return out, nil
}
