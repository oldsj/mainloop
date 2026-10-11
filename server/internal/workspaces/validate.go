package workspaces

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/oldsj/mainloop/server/internal/api"
)

const (
	maxDepth       = 1000
	maxGitNameLen  = 255
	defaultIdleMin = 30
)

var portNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// validGitName applies git's check-ref-format rules to a branch or ref name.
func validGitName(s string) bool {
	if s == "" || len(s) > maxGitNameLen || s == "@" {
		return false
	}
	if strings.ContainsAny(s, " ~^:?*[\\") || strings.Contains(s, "..") ||
		strings.Contains(s, "@{") || strings.Contains(s, "//") {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") ||
		strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") ||
			strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// validatePorts checks the preview ports are well formed and unique by name and number.
func validatePorts(ports []api.WorkspacePort) error {
	names, numbers := map[string]bool{}, map[int]bool{}
	for _, p := range ports {
		if !portNameRE.MatchString(p.Name) || p.Number < 1 || p.Number > 65535 {
			return fmt.Errorf("workspace port %q is not valid", p.Name)
		}
		if names[p.Name] || numbers[p.Number] {
			return fmt.Errorf("workspace port names and numbers must be unique")
		}
		names[p.Name], numbers[p.Number] = true, true
	}
	return nil
}
