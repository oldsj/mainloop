package tasks

import (
	"errors"
	"testing"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/providers"
)

// fakeProfiles is a registry of profiles for ModelChoices, resolved as the real one does.
type fakeProfiles []config.ProviderProfile

func (f fakeProfiles) Profiles() []config.ProviderProfile { return f }

func (f fakeProfiles) Resolve(id, role string, selecting bool) (config.ProviderProfile, error) {
	for _, p := range f {
		if p.ID != id {
			continue
		}
		if _, ok := p.Agents[role]; !ok {
			return p, providers.ErrNoRole
		}
		if selecting && !p.Enabled {
			return p, providers.ErrDisabled
		}
		return p, nil
	}
	return config.ProviderProfile{}, providers.ErrUnknown
}

// TestModelChoicesListWhatACreateCanSelect: a disabled provider, and one with no Agent for a task
// role, are left out; the rest carry the roles they run, their exact models and effort levels,
// and which is the installation's default.
func TestModelChoicesListWhatACreateCanSelect(t *testing.T) {
	// Given four profiles: a default with both roles, a child-only one, a disabled one, and one
	// with an Agent only for a role tasks never use
	agents := func(roles ...string) map[string]config.AgentRef {
		m := map[string]config.AgentRef{}
		for _, r := range roles {
			m[r] = config.AgentRef{}
		}
		return m
	}
	s := &Service{DefaultProfile: "claude", Providers: fakeProfiles{
		{ID: "claude", Enabled: true, Agents: agents("supervisor", "child"), Aliases: []string{"anthropic"},
			Models: []config.ProviderModel{{ID: "claude-haiku-5-5", Label: "Haiku"}}, DefaultModel: "claude-haiku-5-5",
			EffortLevels: []string{"low", "high"}, DefaultEffort: "low"},
		{ID: "codex", Enabled: true, Agents: agents("child")},
		{ID: "off", Enabled: false, Agents: agents("supervisor", "child")},
		{ID: "chat", Enabled: true, Agents: agents("main")},
	}}

	// When the choices are listed
	got := s.ModelChoices()

	// Then only claude and codex are, with their roles, and claude is the default
	if len(got) != 2 || got[0].ProviderProfileID != "claude" || got[1].ProviderProfileID != "codex" {
		t.Fatalf("choices = %+v", got)
	}
	c := got[0]
	if !c.Default || len(c.Roles) != 2 || c.Roles[0] != "supervisor" || c.Roles[1] != "child" ||
		c.Models[0].ID != "claude-haiku-5-5" || c.DefaultModel != "claude-haiku-5-5" || c.DefaultEffort != "low" || c.Aliases[0] != "anthropic" {
		t.Errorf("claude = %+v", c)
	}
	if got[1].Default || len(got[1].Roles) != 1 || got[1].Roles[0] != "child" {
		t.Errorf("codex = %+v", got[1])
	}
	// And each listed model passes the create check on its provider
	for _, ch := range got {
		for _, m := range ch.Models {
			prof, err := s.Providers.Resolve(ch.ProviderProfileID, ch.Roles[0], true)
			if err != nil || validateModelEffort(CreateInput{Model: m.ID, Effort: ch.DefaultEffort}, prof) != nil {
				t.Errorf("%s %s refused: %v", ch.ProviderProfileID, m.ID, errors.Join(err, validateModelEffort(CreateInput{Model: m.ID}, prof)))
			}
		}
	}
}
