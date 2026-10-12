package mcpserver_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
)

func contractFile(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", mcpserver.ContractPath))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func loadContract(t *testing.T) mcpserver.Contract {
	t.Helper()
	var c mcpserver.Contract
	if err := json.Unmarshal(contractFile(t), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// The file is generated from the tool definitions and the policy table; this is the check that
// someone ran go generate after changing either.
func TestContractFileIsCurrent(t *testing.T) {
	want, err := mcpserver.ContractJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contractFile(t), want) {
		t.Fatalf("%s is out of date; run go generate ./internal/mcpserver", mcpserver.ContractPath)
	}
}

// Every tool in the policy table is implemented, and the other way round: BuildContract fails on
// either gap, so the table and the registry cannot drift apart unnoticed.
func TestPolicyTableAndRegistryAgree(t *testing.T) {
	c, err := mcpserver.BuildContract()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range c.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Equal(names, agentauth.AllTools) {
		t.Fatalf("contract tools %v differ from the policy table %v", names, agentauth.AllTools)
	}
}

// Every tool schema forbids additional properties, which the strict decoder relies on.
func TestEveryInputSchemaForbidsExtraProperties(t *testing.T) {
	for _, tool := range loadContract(t).Tools {
		if tool.InputSchema["type"] != "object" || tool.InputSchema["additionalProperties"] != false {
			t.Errorf("%s: input schema must be a closed object", tool.Name)
		}
	}
}

// A row for an actor the policy gives no tools would make the file longer for nothing.
func TestContractListsOnlyActorsWithTools(t *testing.T) {
	for _, row := range loadContract(t).Visibility {
		if len(row.Ordinary) == 0 && len(row.Approval) == 0 {
			t.Errorf("visibility row %+v lists no tools", row)
		}
	}
}
