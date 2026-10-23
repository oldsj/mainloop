package tasks

import (
	"encoding/json"
	"testing"
)

func TestInitialOverridesInheritAndReplace(t *testing.T) {
	model, effort := "previous-model", "high"
	for _, test := range []struct {
		name, payload, model, effort string
	}{
		{"inherit", `{}`, model, effort},
		{"model", `{"model":"replacement"}`, "replacement", effort},
		{"effort", `{"effort":"low"}`, model, "low"},
		{"both", `{"model":"replacement","effort":"low"}`, "replacement", "low"},
		{"null", `{"model":null,"effort":null}`, model, effort},
	} {
		t.Run(test.name, func(t *testing.T) {
			gotModel, gotEffort, err := initialOverrides(json.RawMessage(test.payload), &model, &effort)
			if err != nil || gotModel == nil || gotEffort == nil || *gotModel != test.model || *gotEffort != test.effort {
				t.Fatalf("overrides = %v, %v, %v", gotModel, gotEffort, err)
			}
		})
	}
	gotModel, gotEffort, err := initialOverrides(json.RawMessage(`{}`), nil, nil)
	if err != nil || gotModel != nil || gotEffort != nil {
		t.Fatalf("defaults = %v, %v, %v", gotModel, gotEffort, err)
	}
}
