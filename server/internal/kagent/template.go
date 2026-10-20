package kagent

import (
	"context"
	"errors"

	"google.golang.org/protobuf/types/known/structpb"

	api "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

// ErrNoTemplate means an Agent has no named template reference: it carries an inline template, both
// forms, or neither. Such an Agent is not one the operator reviewed by name.
var ErrNoTemplate = errors.New("kagent: agent has no named templateRef")

// SessionAgent reads a Session and returns the Agent it runs on.
func (c *Client) SessionAgent(ctx context.Context, sessionID string) (AgentRef, error) {
	s, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return AgentRef{}, err
	}
	a := s.GetAgent()
	if a.GetNamespace() == "" || a.GetName() == "" {
		return AgentRef{}, errors.New("kagent: session names no agent")
	}
	return AgentRef{Namespace: a.GetNamespace(), Name: a.GetName()}, nil
}

// AgentTemplate reads an Agent and returns the name of its templateRef. An inline template, both
// forms, or neither return ErrNoTemplate.
func (c *Client) AgentTemplate(ctx context.Context, ref AgentRef) (string, error) {
	a, err := c.GetAgent(ctx, ref)
	if err != nil {
		return "", err
	}
	return templateName(a)
}

func templateName(a *api.Agent) (string, error) {
	spec := a.GetResource().GetValue().GetFields()["spec"].GetStructValue()
	if spec == nil {
		return "", ErrNoTemplate
	}
	inline := spec.GetFields()["template"]
	ref := spec.GetFields()["templateRef"]
	if isSet(inline) == isSet(ref) {
		return "", ErrNoTemplate
	}
	name := ref.GetStructValue().GetFields()["name"].GetStringValue()
	if name == "" {
		return "", ErrNoTemplate
	}
	return name, nil
}

func isSet(v *structpb.Value) bool {
	if v == nil {
		return false
	}
	_, null := v.GetKind().(*structpb.Value_NullValue)
	return !null
}
