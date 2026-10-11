package workspaces

import (
	"fmt"
	"regexp"

	"github.com/oldsj/mainloop/server/internal/api"
)

const (
	maxDepth       = 1000
	defaultIdleMin = 30
)

var portNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

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
