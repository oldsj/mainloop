package mcpserver

import (
	"strings"
	"testing"
)

// TestCheckToolsImplementedRefusesAPolicyNameMissingFromTheRegistry guards the panic Handler
// construction now prevents: were this check absent, buildServer would register a zero Tool for
// the missing name, whose nil Handle panics on the first call.
func TestCheckToolsImplementedRefusesAPolicyNameMissingFromTheRegistry(t *testing.T) {
	implemented := map[string]Tool{"a": {Name: "a"}, "b": {Name: "b"}}
	if err := checkToolsImplemented(implemented, []string{"a", "b"}); err != nil {
		t.Fatalf("matching sets: err = %v", err)
	}
	err := checkToolsImplemented(implemented, []string{"a", "b", "c"})
	if err == nil || !strings.Contains(err.Error(), "c") {
		t.Fatalf("a policy name missing from the registry: err = %v", err)
	}
}
