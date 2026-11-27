package dotfiles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ClaudeSettingsPath is the dotfiles file that carries the owner's Claude auto mode and permission
// rules. kagent merges it under the harness's own settings for Claude Sessions on the SDK driver.
const ClaudeSettingsPath = "claude/settings.json"

// claudeSettingKeys are the only keys claude/settings.json may hold, per top-level object. Each
// is a list of rule strings. Everything else in Claude's settings (modes, hooks, env, MCP) stays
// the harness's, so it is refused here rather than silently dropped.
var claudeSettingKeys = map[string][]string{
	"autoMode":    {"environment", "allow", "soft_deny", "hard_deny"},
	"permissions": {"allow", "deny", "ask"},
}

// CheckClaudeSettings reports why data is not a claude/settings.json the export takes: a JSON
// object with only `$schema`, `autoMode.{environment,allow,soft_deny,hard_deny}` and
// `permissions.{allow,deny,ask}`, each list holding non-empty strings. The error names keys,
// never values.
func CheckClaudeSettings(data []byte) error {
	var settings map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&settings); err != nil || settings == nil {
		return fmt.Errorf("%s is not a JSON object", ClaudeSettingsPath)
	}
	if decoder.More() {
		return fmt.Errorf("%s has data after its object", ClaudeSettingsPath)
	}
	for top, value := range settings {
		if top == "$schema" {
			continue
		}
		allowed, ok := claudeSettingKeys[top]
		if !ok {
			return fmt.Errorf("%s may not set %q", ClaudeSettingsPath, top)
		}
		var section map[string]json.RawMessage
		if err := json.Unmarshal(value, &section); err != nil || section == nil {
			return fmt.Errorf("%s %s is not an object", ClaudeSettingsPath, top)
		}
		for key, list := range section {
			if !slices.Contains(allowed, key) {
				return fmt.Errorf("%s may not set %q", ClaudeSettingsPath, top+"."+key)
			}
			var rules []string
			if err := json.Unmarshal(list, &rules); err != nil {
				return fmt.Errorf("%s %s.%s is not a list of strings", ClaudeSettingsPath, top, key)
			}
			for _, rule := range rules {
				if strings.TrimSpace(rule) == "" {
					return fmt.Errorf("%s %s.%s has an empty entry", ClaudeSettingsPath, top, key)
				}
			}
		}
	}
	return nil
}
