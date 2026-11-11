package sessions

import (
	"maps"
	"testing"

	"github.com/oldsj/mainloop/server/internal/kagent"
)

// Without a profile there is no default to name: only an override is sent, and with none the turn
// carries no model or effort, so kagent's harness default applies.
func TestTurnMetadataWithoutAProfileNamesOnlyOverrides(t *testing.T) {
	s := &Service{}
	if got := s.turnMetadata("unknown", nil, nil); got != nil {
		t.Fatalf("no override: %+v, want nil", got)
	}
	model := "some-model"
	want := map[string]any{kagent.ModelMetadataKey: model}
	if got := s.turnMetadata("unknown", &model, nil); !maps.Equal(got, want) {
		t.Fatalf("model override: %+v, want %+v", got, want)
	}
}
