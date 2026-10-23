package agentcontext_test

import (
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentcontext"
	"github.com/oldsj/mainloop/server/internal/config"
)

func TestDelegationChoicesComeFromProfile(t *testing.T) {
	got := agentcontext.DelegationProfiles([]config.ProviderProfile{
		{ID: "test", Enabled: true, Models: []config.ProviderModel{{ID: "custom-small"}}, DefaultModel: "custom-small", EffortLevels: []string{"low", "high"}, DefaultEffort: "low"},
		{ID: "disabled", Models: []config.ProviderModel{{ID: "hidden-model"}}},
	})
	for _, want := range []string{"custom-small", "effort_levels=[low high]", "default=low", "scope", "risk", "reversibility", "mechanical", "judgment", "cheapest", "higher tier"} {
		if !strings.Contains(got, want) {
			t.Errorf("guidance lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "hidden-model") {
		t.Fatal("disabled profile offered")
	}
}
