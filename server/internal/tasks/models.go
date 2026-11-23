package tasks

import (
	"slices"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/providers"
)

// ModelChoice is one provider a delegate may name, with the exact models and effort levels it
// accepts for it (docs/specs/delegation.md#list-models).
type ModelChoice struct {
	ProviderProfileID string
	DisplayName       string
	Aliases           []string
	// Roles are the task roles it runs: supervisor (a root task) and child (a supervisor's task).
	Roles         []string
	Models        []config.ProviderModel
	DefaultModel  string
	EffortLevels  []string
	DefaultEffort string
	// Default marks the installation's default provider, used when neither the task nor its
	// project names one.
	Default bool
}

// ModelChoices lists the providers a create can select, from the registry and with the checks
// [Service.selectProfile] and [validateModelEffort] apply: enabled, with an Agent for a task role,
// and the profile's own models and effort levels. The registry is the one source, so a model
// listed here is one a delegate on that provider accepts.
func (s *Service) ModelChoices() []ModelChoice {
	var out []ModelChoice
	for _, p := range s.Providers.Profiles() {
		var roles []string
		for _, role := range []string{providers.RoleSupervisor, roleChild} {
			if _, err := s.Providers.Resolve(p.ID, role, true); err == nil {
				roles = append(roles, role)
			}
		}
		if len(roles) == 0 {
			continue
		}
		out = append(out, ModelChoice{
			ProviderProfileID: p.ID, DisplayName: p.DisplayName, Aliases: slices.Clone(p.Aliases), Roles: roles,
			Models: slices.Clone(p.Models), DefaultModel: p.DefaultModel,
			EffortLevels: slices.Clone(p.EffortLevels), DefaultEffort: p.DefaultEffort,
			Default: p.ID == s.DefaultProfile,
		})
	}
	return out
}
