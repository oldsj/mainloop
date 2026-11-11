package kagent

import (
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	api "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

func agentWithSpec(t *testing.T, spec map[string]any) *api.Agent {
	t.Helper()
	v, err := structpb.NewStruct(map[string]any{"spec": spec})
	if err != nil {
		t.Fatal(err)
	}
	return &api.Agent{Resource: &api.StructuredObject{Value: v}}
}

// Only a named template ref (v1alpha3 spec.template.ref.name) gives merge authority: an inline
// template, both forms, neither, an empty name, or the retired 0.4 spec.templateRef do not.
func TestTemplateName(t *testing.T) {
	got, err := templateName(agentWithSpec(t, map[string]any{
		"template":   map[string]any{"ref": map[string]any{"name": "claude-workspace-template"}},
		"harnessRef": map[string]any{"name": "claude-workspace-harness"},
	}))
	if err != nil || got != "claude-workspace-template" {
		t.Fatalf("named ref = %q, %v", got, err)
	}
	for name, spec := range map[string]map[string]any{
		"inline":       {"template": map[string]any{"inline": map[string]any{"x": "y"}}},
		"both":         {"template": map[string]any{"inline": map[string]any{"x": "y"}, "ref": map[string]any{"name": "t"}}},
		"neither":      {"template": map[string]any{}},
		"no template":  {},
		"no name":      {"template": map[string]any{"ref": map[string]any{}}},
		"0.4 shape":    {"templateRef": map[string]any{"name": "t"}},
		"not a struct": {"template": "t"},
	} {
		if _, err := templateName(agentWithSpec(t, spec)); err == nil {
			t.Errorf("%s: want ErrNoTemplate", name)
		}
	}
	if _, err := templateName(&api.Agent{}); err == nil {
		t.Error("no resource: want ErrNoTemplate")
	}
}
