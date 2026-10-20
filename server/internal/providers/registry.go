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
// otherwise is added. A replacing profile with the same native_provider as the built-in inherits
// the built-in's Models, DefaultModel, EffortLevels and DefaultEffort for any of those it leaves
// empty; it keeps its own values for any it sets. Any malformed profile, including a built-in one
// made from a bad KAGENT_* setting, or any duplicate ID or alias, is an error.
func NewRegistry(k config.Kagent) (*Registry, error) {
	profiles := builtins(k)
	index := map[string]int{}
	for i, p := range profiles {
		index[p.ID] = i
	}
	for _, p := range k.ProviderProfiles {
		if i, ok := index[p.ID]; ok {
			profiles[i] = inheritBuiltinModelAndEffort(p, profiles[i])
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

// inheritBuiltinModelAndEffort fills an operator profile's empty Models/DefaultModel and
// EffortLevels/DefaultEffort from the built-in profile it replaces, when both name the same
// native_provider. A profile that sets either pair explicitly keeps its own values: validation
// already requires DefaultModel/DefaultEffort to be empty whenever Models/EffortLevels is, so
// "empty" here only matches an operator profile that left the pair unset.
func inheritBuiltinModelAndEffort(operator, builtin config.ProviderProfile) config.ProviderProfile {
	if operator.NativeProvider != builtin.NativeProvider {
		return operator
	}
	if len(operator.Models) == 0 {
		operator.Models = builtin.Models
		operator.DefaultModel = builtin.DefaultModel
	}
	if len(operator.EffortLevels) == 0 {
		operator.EffortLevels = builtin.EffortLevels
		operator.DefaultEffort = builtin.DefaultEffort
	}
	return operator
}

// claudeModels and codexModels are the built-in Claude and Codex profiles' /model listings. An
// operator profile sets its own in PROVIDER_PROFILES; these are Mainloop's defaults only.
//
// Sourcing note: this task's sandbox cannot reach the oldsj/kagent fork (GitHub) or Anthropic's/
// OpenAI's own docs (platform.claude.com, code.claude.com, developers.openai.com all returned
// 403), so the runtime Dockerfiles for the claude-runtime and codex-runtime images pinning Claude
// Code 2.1.260 and Codex 0.148.0 could not be inspected directly. The lists below reflect
// Anthropic's and OpenAI's published model catalogs as corroborated across multiple independent
// sources found via web search: Claude's current family is Opus 5.5 (claude-opus-5-5), Sonnet 5.5
// (claude-sonnet-5-5), Haiku 5.5 (claude-haiku-5-5) and Fable 5.1 (claude-fable-5-1, unchanged);
// Codex's is still gpt-5.1-codex and gpt-5.1-codex-mini, as the 0.148.0 release notes (Aug 18,
// 2026: /export, session forking, Bedrock provider support) list no model-catalog change. Several
// sources place Opus 5.5's "default on Pro/Team Standard" switch in a later Claude Code release
// than 2.1.260, so the CLI's own live default for that pinned version could not be confirmed;
// per the task, the default stays with the Sonnet family rather than moving to Opus.
var (
	claudeModels = []config.ProviderModel{
		{ID: "claude-sonnet-5-5", Label: "Sonnet 5.5"},
		{ID: "claude-opus-5-5", Label: "Opus 5.5"},
		{ID: "claude-haiku-5-5", Label: "Haiku 5.5"},
		{ID: "claude-fable-5-1", Label: "Fable 5.1"},
	}
	claudeEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}
	codexModels        = []config.ProviderModel{
		{ID: "gpt-5.1-codex", Label: "GPT-5.1 Codex"},
		{ID: "gpt-5.1-codex-mini", Label: "GPT-5.1 Codex Mini"},
	}
	codexEffortLevels = []string{"minimal", "low", "medium", "high"}
)

func builtins(k config.Kagent) []config.ProviderProfile {
	ref := func(name string) config.AgentRef { return config.AgentRef{Namespace: k.Namespace, Name: name} }
	build := func(
		kind, display, main, supervisor, child, workspace string,
		models []config.ProviderModel, defaultModel string, effortLevels []string, defaultEffort string,
	) config.ProviderProfile {
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
			Aliases:       []string{},
			Enabled:       true,
			Capabilities:  caps,
			Models:        models,
			DefaultModel:  defaultModel,
			EffortLevels:  effortLevels,
			DefaultEffort: defaultEffort,
		}
	}
	return []config.ProviderProfile{
		build(config.NativeClaude, "Claude Code", k.MainAgent, k.SupervisorClaudeAgent, k.ClaudeAgent, k.WorkspaceClaudeAgent,
			claudeModels, claudeModels[0].ID, claudeEffortLevels, "medium"),
		build(config.NativeCodex, "Codex", k.MainCodexAgent, k.SupervisorCodexAgent, k.CodexAgent, k.WorkspaceCodexAgent,
			codexModels, codexModels[0].ID, codexEffortLevels, "medium"),
	}
}

// Profiles returns the profiles in display order: the built-ins, then operator additions.
func (r *Registry) Profiles() []config.ProviderProfile {
	out := make([]config.ProviderProfile, len(r.profiles))
	copy(out, r.profiles)
	return out
}

// Profile finds a profile by ID or alias, with no role or enabled check: a caller that only needs
// its identity (native provider, models, effort levels), not a chance to run a turn, uses this
// instead of Resolve.
func (r *Registry) Profile(identifier string) (config.ProviderProfile, bool) {
	i, ok := r.byName[identifier]
	if !ok {
		return config.ProviderProfile{}, false
	}
	return r.profiles[i], true
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
