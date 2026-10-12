package mcpserver

import (
	"strings"
	"testing"
)

func TestContractToolsRefusesATableThatDisagreesWithTheRegistry(t *testing.T) {
	implemented := []Tool{{Name: "a"}, {Name: "b"}}
	if got, err := contractTools(implemented, []string{"b", "a"}); err != nil || len(got) != 2 || got[0].Name != "b" {
		t.Fatalf("matching sets: got %v, %v", got, err)
	}
	if _, err := contractTools(implemented, []string{"a", "b", "c"}); err == nil || !strings.Contains(err.Error(), `"c"`) {
		t.Errorf("a policy tool that is not implemented: err = %v", err)
	}
	if _, err := contractTools(implemented, []string{"a"}); err == nil || !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("an implemented tool missing from the policy table: err = %v", err)
	}
}
