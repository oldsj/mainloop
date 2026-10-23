package agentcontext

import (
	"fmt"
	"strings"

	"github.com/oldsj/mainloop/server/internal/config"
)

const DelegationGuidance = "When delegating, choose model and effort from the provider profile's models and effort_levels. " +
	"Match scope, risk and reversibility: use a cheaper model and lower effort for bounded mechanical work; " +
	"use a stronger model and higher effort for judgment, design, debugging or security-sensitive work. " +
	"Prefer the cheapest choice likely to succeed. When re-delegating a failure, consider a higher tier.\n"

func DelegationProfiles(profiles []config.ProviderProfile) string {
	var text strings.Builder
	text.WriteString("\n## Delegation choices\n")
	text.WriteString(DelegationGuidance)
	for _, profile := range profiles {
		if !profile.Enabled {
			continue
		}
		models := make([]string, 0, len(profile.Models))
		for _, model := range profile.Models {
			models = append(models, model.ID)
		}
		fmt.Fprintf(&text, "- %s: models=%v (default=%s); effort_levels=%v (default=%s)\n", profile.ID, models, profile.DefaultModel, profile.EffortLevels, profile.DefaultEffort)
	}
	return text.String()
}
