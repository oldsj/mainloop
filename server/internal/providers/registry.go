// Package providers is the server-owned registry of native provider profiles, and the project
// default-provider preference. There is no discovery and no caller-supplied wiring: the registry
// is the built-in Claude and Codex profiles plus the validated PROVIDER_PROFILES list.
package providers

import (
	"errors"
	"fmt"

	"github.com/oldsj/mainloop/server/internal/config"
)

// Capabilities is every capability a profile reports evidence for, in display order.
var Capabilities = []string{
	"native_create",
	"native_resume",
	"native_identity",
	"message_receipt",
	"message_completion",
	"cancel_reconciliation",
	"workspace_git_origins",
	"workspace_environment_composition",
	"workspace_snapshot",
	"workspace_freeze",
	"workspace_import",
	"mainloop_mcp_credentials",
	"hitl",
	"retention",
}

// Roles a profile can name an Agent for.
const (
	RoleSupervisor = "supervisor"
)

// Registry resolves provider IDs and aliases to profiles. It is immutable after [NewRegistry].
type Registry struct {
	profiles []config.ProviderProfile
	byName   map[string]int
}

// Resolve failures. Callers show the message; match with errors.Is.
var (
	ErrUnknown  = errors.New("provider profile is not configured")
	ErrNoRole   = errors.New("provider profile has no Agent for the role")
	ErrDisabled = errors.New("provider profile is disabled")
)

// resolveError carries the message shown to the caller and the sentinel to match on.
type resolveError struct {
	msg  string
	kind error
}

func (e resolveError) Error() string { return e.msg }
func (e resolveError) Unwrap() error { return e.kind }

// NewRegistry builds the registry from the built-in profiles and the operator's
// PROVIDER_PROFILES. An operator profile replaces the built-in profile with the same ID and
// otherwise is added. Any malformed profile, including a built-in one made from a bad KAGENT_*
// setting, or any duplicate ID or alias, is an error.
func NewRegistry(k config.Kagent) (*Registry, error) {
	profiles := builtins(k)
	index := map[string]int{}
	for i, p := range profiles {
		index[p.ID] = i
	}
	for _, p := range k.ProviderProfiles {
		if i, ok := index[p.ID]; ok {
			profiles[i] = p
			continue
		}
		index[p.ID] = len(profiles)
		profiles = append(profiles, p)
	}
	r := &Registry{profiles: profiles, byName: map[string]int{}}
	for i, p := range profiles {
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("provider registry: %w", err)
		}
		for _, name := range append([]string{p.ID}, p.Aliases...) {
			if _, dup := r.byName[name]; dup {
				return nil, fmt.Errorf("provider registry: duplicate provider ID or alias %q", name)
			}
			r.byName[name] = i
		}
	}
	return r, nil
}

func builtins(k config.Kagent) []config.ProviderProfile {
	ref := func(name string) config.AgentRef { return config.AgentRef{Namespace: k.Namespace, Name: name} }
	build := func(kind, display, main, supervisor, child, workspace string) config.ProviderProfile {
		caps := make([]config.CapabilityResult, len(Capabilities))
		for i, name := range Capabilities {
			caps[i] = config.CapabilityResult{Capability: name, State: "unknown", Scope: "unverified"}
		}
		return config.ProviderProfile{
			ID:                    kind,
			DisplayName:           display,
			RuntimeAdapter:        "kagent",
			NativeProvider:        kind,
			ConfigurationRevision: "native-v1",
			Agents: map[string]config.AgentRef{
				"main":       ref(main),
				"supervisor": ref(supervisor),
				"child":      ref(child),
				"agent":      ref(workspace),
			},
			Aliases:      []string{},
			Enabled:      true,
			Capabilities: caps,
		}
	}
	return []config.ProviderProfile{
		build(config.NativeClaude, "Claude Code", k.MainAgent, k.SupervisorClaudeAgent, k.ClaudeAgent, k.WorkspaceClaudeAgent),
		build(config.NativeCodex, "Codex", k.MainCodexAgent, k.SupervisorCodexAgent, k.CodexAgent, k.WorkspaceCodexAgent),
	}
}

// Profiles returns the profiles in display order: the built-ins, then operator additions.
func (r *Registry) Profiles() []config.ProviderProfile {
	out := make([]config.ProviderProfile, len(r.profiles))
	copy(out, r.profiles)
	return out
}

// Resolve finds a profile by ID or alias. The profile must have an Agent for role. When
// selecting, a disabled profile is rejected too: disabled profiles stay listed and resolvable for
// work already pinned to them.
func (r *Registry) Resolve(identifier, role string, selecting bool) (config.ProviderProfile, error) {
	i, ok := r.byName[identifier]
	if !ok {
		return config.ProviderProfile{}, resolveError{fmt.Sprintf("no provider profile is configured for %q", identifier), ErrUnknown}
	}
	p := r.profiles[i]
	if _, ok := p.Agents[role]; !ok {
		return config.ProviderProfile{}, resolveError{fmt.Sprintf("provider %q has no %q Agent", identifier, role), ErrNoRole}
	}
	if selecting && !p.Enabled {
		return config.ProviderProfile{}, resolveError{fmt.Sprintf("provider %q is disabled", identifier), ErrDisabled}
	}
	return p, nil
}
