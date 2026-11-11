package sessions

import (
	"maps"
	"testing"

	"github.com/oldsj/mainloop/server/internal/kagent"
)

// Only what was chosen is sent: with no override the turn names no model or effort, so kagent uses
// the Agent's ModelConfig and the harness default.
func TestTurnMetadataNamesOnlyOverrides(t *testing.T) {
	if got := turnMetadata(nil, nil); got != nil {
		t.Fatalf("no override: %+v, want nil", got)
	}
	model, effort := "some-model", "high"
	if got, want := turnMetadata(&model, nil), map[string]any{kagent.ModelMetadataKey: model}; !maps.Equal(got, want) {
		t.Fatalf("model override: %+v, want %+v", got, want)
	}
	if got, want := turnMetadata(nil, &effort), map[string]any{kagent.EffortMetadataKey: effort}; !maps.Equal(got, want) {
		t.Fatalf("effort override: %+v, want %+v", got, want)
	}
	want := map[string]any{kagent.ModelMetadataKey: model, kagent.EffortMetadataKey: effort}
	if got := turnMetadata(&model, &effort); !maps.Equal(got, want) {
		t.Fatalf("both: %+v, want %+v", got, want)
	}
}
