package dotfiles_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/dotfiles"
)

// Fictional owner settings: auto mode rules and permission rules only.
const ownerSettings = `{
  "$schema": "https://json.schemastore.org/claude-code-settings.json",
  "autoMode": {
    "environment": ["$defaults", "Trusted repo: github.com/example/fictional"],
    "soft_deny": ["$defaults", "Deleting example buckets"],
    "allow": [], "hard_deny": ["$defaults"]
  },
  "permissions": {"allow": ["Bash(make test)"], "deny": ["Read(**/.env)"], "ask": ["Bash(example deploy *)"]}
}`

func TestSelectTakesClaudeSettingsAlone(t *testing.T) {
	files, err := dotfiles.Select(bytes.NewReader(archive(t,
		entry{name: "home/.agents/claude/settings.json", body: ownerSettings},
		entry{name: "home/.agents/claude/settings.local.json", body: "{}"},
		entry{name: "home/.agents/claude/other.json", body: "{}"},
	)), "home/.agents", dotfiles.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(files); !slices.Equal(got, []string{dotfiles.ClaudeSettingsPath}) {
		t.Fatalf("selected %v", got)
	}
}

func TestSelectRefusesClaudeSettingsBeyondTheRuleLists(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":             `{"autoMode":`,
		"not an object":        `["autoMode"]`,
		"null":                 `null`,
		"trailing data":        `{} {}`,
		"a mode":               `{"permissions": {"defaultMode": "bypassPermissions"}}`,
		"the bypass lock":      `{"permissions": {"disableBypassPermissionsMode": "allow"}}`,
		"hooks":                `{"hooks": {"PreToolUse": []}}`,
		"env":                  `{"env": {"EXAMPLE": "x"}}`,
		"another autoMode key": `{"autoMode": {"classifyAllShell": true}}`,
		"section not object":   `{"autoMode": ["x"]}`,
		"list not strings":     `{"permissions": {"deny": [1]}}`,
		"empty entry":          `{"autoMode": {"environment": [" "]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := dotfiles.Select(bytes.NewReader(archive(t,
				entry{name: "AGENTS.md", body: "x"},
				entry{name: "claude/settings.json", body: body},
			)), "", dotfiles.DefaultLimits)
			if got := refusal(t, err); got != dotfiles.ReasonInvalidSettings {
				t.Fatalf("reason = %q, want %q", got, dotfiles.ReasonInvalidSettings)
			}
		})
	}
}

func TestCheckClaudeSettingsNamesKeysNotValues(t *testing.T) {
	if err := dotfiles.CheckClaudeSettings([]byte(ownerSettings)); err != nil {
		t.Fatal(err)
	}
	err := dotfiles.CheckClaudeSettings([]byte(`{"env": {"EXAMPLE_TOKEN": "fictional-value-123"}}`))
	if err == nil || !strings.Contains(err.Error(), `"env"`) || strings.Contains(err.Error(), "fictional-value-123") {
		t.Fatalf("error = %v", err)
	}
}
