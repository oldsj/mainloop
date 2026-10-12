package kagent

import (
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	api "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

func agentWithSpec(t *testing.T, spec map[string]any) *api.Agent {
	t.Helper()
	v, err := structpb.NewStruct(map[string]any{"spec": spec})
	if err != nil {
		t.Fatal(err)
	}
	return &api.Agent{Resource: &api.StructuredObject{Value: v}}
}

// Only a named templateRef gives merge authority: an inline template, both forms, neither, or an
// empty name do not.
func TestTemplateName(t *testing.T) {
	got, err := templateName(agentWithSpec(t, map[string]any{"templateRef": map[string]any{"name": "claude-workspace-template"}}))
	if err != nil || got != "claude-workspace-template" {
		t.Fatalf("named ref = %q, %v", got, err)
	}
	for name, spec := range map[string]map[string]any{
		"inline":  {"template": map[string]any{"x": "y"}},
		"both":    {"template": map[string]any{"x": "y"}, "templateRef": map[string]any{"name": "t"}},
		"neither": {},
		"no name": {"templateRef": map[string]any{}},
	} {
		if _, err := templateName(agentWithSpec(t, spec)); err == nil {
			t.Errorf("%s: want ErrNoTemplate", name)
		}
	}
	if _, err := templateName(&api.Agent{}); err == nil {
		t.Error("no resource: want ErrNoTemplate")
	}
}
