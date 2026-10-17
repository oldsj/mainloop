package agentcontext_test

import (
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentcontext"
)

// maxGuidanceBytes keeps the shared guidance short enough to read at the start of every session.
const maxGuidanceBytes = 2048

func TestGuidanceIsBounded(t *testing.T) {
	if n := len(agentcontext.Guidance); n > maxGuidanceBytes {
		t.Fatalf("guidance is %d bytes, want at most %d", n, maxGuidanceBytes)
	}
}

func TestGuidanceNamesItsVersion(t *testing.T) {
	if want := "(guidance v1)"; agentcontext.Version != 1 || !strings.Contains(agentcontext.Guidance, want) {
		t.Fatalf("version %d does not match the heading %q", agentcontext.Version, want)
	}
}

func TestGuidanceCoversTheEnvironment(t *testing.T) {
	for _, want := range []string{
		"isolated", "`/data/workspace`", "survives a pause", "`/data`", "`/opt/cache`", "`GOCACHE`",
		"do not move them", "AGENTS.md and CLAUDE.md", "smallest checks", "pull request", "no production or live commands",
		"evidence", "`owner_action`", "`/health`",
	} {
		if !strings.Contains(strings.ToLower(agentcontext.Guidance), strings.ToLower(want)) {
			t.Errorf("guidance lacks %q", want)
		}
	}
	for _, line := range strings.Split(agentcontext.Guidance, "\n") {
		if strings.HasPrefix(line, "[mainloop:") {
			t.Errorf("a guidance line reads as a preamble marker: %q", line)
		}
	}
}
