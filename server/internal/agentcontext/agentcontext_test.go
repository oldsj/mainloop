package agentcontext_test

import (
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentcontext"
)

// maxGuidanceBytes keeps the shared guidance short enough to read at the start of every session.
const maxGuidanceBytes = 1536

func TestGuidanceIsBounded(t *testing.T) {
	if n := len(agentcontext.Guidance); n > maxGuidanceBytes {
		t.Fatalf("guidance is %d bytes, want at most %d", n, maxGuidanceBytes)
	}
}

func TestGuidanceNamesItsVersion(t *testing.T) {
	if want := "(guidance v9)"; agentcontext.Version != 9 || !strings.Contains(agentcontext.Guidance, want) {
		t.Fatalf("version %d does not match the heading %q", agentcontext.Version, want)
	}
}

func TestGuidanceCoversTheEnvironment(t *testing.T) {
	for _, want := range []string{
		"isolated", "`/data/workspace`", "survives a pause", "`/data`", "AGENTS.md and CLAUDE.md", "no `gh`",
		"no production commands", "evidence", "`owner_action`", "directly in chat", "`/health`", "`remember`", "`forget`",
		"proposals the owner decides", "`create_issue`", "`comment_issue`",
	} {
		if !strings.Contains(strings.ToLower(agentcontext.Guidance), strings.ToLower(want)) {
			t.Errorf("guidance lacks %q", want)
		}
	}
	// Coordination tasks get this text too, and have none of these tools: the code brief carries them.
	for _, codeOnly := range []string{"get_verification", "watch_checks", "dev_", "open_pull_request", "merge_pull_request", "/opt/cache"} {
		if strings.Contains(agentcontext.Guidance, codeOnly) {
			t.Errorf("guidance names %q, which only code tasks have", codeOnly)
		}
	}
	for _, line := range strings.Split(agentcontext.Guidance, "\n") {
		if strings.HasPrefix(line, "[mainloop:") {
			t.Errorf("a guidance line reads as a preamble marker: %q", line)
		}
	}
}
