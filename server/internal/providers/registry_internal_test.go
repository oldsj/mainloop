package providers

import (
	"slices"
	"testing"

	"github.com/oldsj/mainloop/server/internal/config"
)

// TestInheritBuiltinModelAndEffortRequiresMatchingNativeProvider is a white-box test of the
// registry's merge helper: it never inherits across a native_provider change, even when the
// operator profile otherwise leaves models/effort empty.
func TestInheritBuiltinModelAndEffortRequiresMatchingNativeProvider(t *testing.T) {
	builtin := config.ProviderProfile{
		ID:             "claude",
		NativeProvider: config.NativeClaude,
		Models:         []config.ProviderModel{{ID: "m1", Label: "M1"}},
		DefaultModel:   "m1",
		EffortLevels:   []string{"low", "high"},
		DefaultEffort:  "low",
	}
	operator := config.ProviderProfile{
		ID:             "claude",
		NativeProvider: config.NativeCodex, // differs from the built-in it is replacing
	}

	got := inheritBuiltinModelAndEffort(operator, builtin)

	if len(got.Models) != 0 || got.DefaultModel != "" || len(got.EffortLevels) != 0 || got.DefaultEffort != "" {
		t.Errorf("inherited across a native_provider mismatch: %+v", got)
	}
}

func TestInheritBuiltinModelAndEffortFillsOnlyWhatIsEmpty(t *testing.T) {
	builtin := config.ProviderProfile{
		ID:             "claude",
		NativeProvider: config.NativeClaude,
		Models:         []config.ProviderModel{{ID: "m1", Label: "M1"}},
		DefaultModel:   "m1",
		EffortLevels:   []string{"low", "high"},
		DefaultEffort:  "low",
	}
	operator := config.ProviderProfile{
		ID:             "claude",
		NativeProvider: config.NativeClaude,
		EffortLevels:   []string{"high"},
		DefaultEffort:  "high",
	}

	got := inheritBuiltinModelAndEffort(operator, builtin)

	if !slices.Equal(modelIDsOf(got.Models), []string{"m1"}) || got.DefaultModel != "m1" {
		t.Errorf("did not inherit empty models: %+v", got)
	}
	if !slices.Equal(got.EffortLevels, []string{"high"}) || got.DefaultEffort != "high" {
		t.Errorf("overwrote the operator's own effort levels: %+v", got)
	}
}

func modelIDsOf(ms []config.ProviderModel) []string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	return ids
}
