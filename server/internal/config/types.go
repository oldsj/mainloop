package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Seconds is a duration configured as a (possibly fractional) number of seconds, the format
// every *_SECONDS variable already uses.
type Seconds time.Duration

// Duration returns the value as a time.Duration.
func (s Seconds) Duration() time.Duration { return time.Duration(s) }

// UnmarshalText parses a positive decimal number of seconds.
func (s *Seconds) UnmarshalText(text []byte) error {
	v, err := strconv.ParseFloat(strings.TrimSpace(string(text)), 64)
	if err != nil || !(v > 0 && v <= 1e9) {
		return errors.New("must be a positive number of seconds")
	}
	*s = Seconds(time.Duration(v * float64(time.Second)))
	return nil
}

// decodeStrict decodes one JSON value into v, rejecting unknown fields and trailing data.
// Errors never quote the input, so configuration values cannot leak into logs.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return sanitizeJSONError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON value")
	}
	return nil
}

// sanitizeJSONError keeps the error class and field name but drops anything quoting input.
func sanitizeJSONError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return fmt.Errorf("field %q must be a %s", typeErr.Field, typeErr.Type)
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Errorf("invalid JSON at offset %d", syntaxErr.Offset)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("invalid JSON: unexpected end of input")
	}
	// Unknown-field errors name only the field; other decoder errors carry no values.
	return err
}

var (
	providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	kagentNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
)

// Provider and agent role names.
const (
	NativeClaude = "claude"
	NativeCodex  = "codex"
)

var providerRoles = map[string]bool{"main": true, "supervisor": true, "child": true, "agent": true}

// AgentRef names one kagent Agent.
type AgentRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// CapabilityResult is scoped evidence for one provider capability.
type CapabilityResult struct {
	Capability  string  `json:"capability"`
	State       string  `json:"state"`
	Scope       string  `json:"scope"`
	EvidenceRef *string `json:"evidence_ref"`
	Detail      *string `json:"detail"`
}

// UnmarshalJSON applies the field defaults before strict decoding.
func (c *CapabilityResult) UnmarshalJSON(data []byte) error {
	type plain CapabilityResult
	v := plain{State: "unknown", Scope: "unverified"}
	if err := decodeStrict(data, &v); err != nil {
		return err
	}
	*c = CapabilityResult(v)
	return nil
}

func (c CapabilityResult) validate() error {
	if c.Capability == "" {
		return errors.New("capability must not be empty")
	}
	switch c.State {
	case "proved", "partial", "unsupported", "unknown":
	default:
		return errors.New("capability state must be proved, partial, unsupported or unknown")
	}
	switch c.Scope {
	case "fixture", "live", "unverified":
	default:
		return errors.New("capability scope must be fixture, live or unverified")
	}
	if c.EvidenceRef != nil && *c.EvidenceRef == "" {
		return errors.New("evidence_ref must not be empty")
	}
	if (c.State == "proved" || c.State == "partial") && (c.EvidenceRef == nil || c.Scope == "unverified") {
		return errors.New("proved/partial capabilities require scoped evidence")
	}
	return nil
}

// ProviderProfile is an operator-owned native provider profile (PROVIDER_PROFILES entry).
type ProviderProfile struct {
	ID                    string              `json:"id"`
	DisplayName           string              `json:"display_name"`
	RuntimeAdapter        string              `json:"runtime_adapter"`
	NativeProvider        string              `json:"native_provider"`
	ConfigurationRevision string              `json:"configuration_revision"`
	Agents                map[string]AgentRef `json:"agents"`
	Aliases               []string            `json:"aliases"`
	Enabled               bool                `json:"enabled"`
	Capabilities          []CapabilityResult  `json:"capabilities"`
}

// UnmarshalJSON applies the field defaults before strict decoding.
func (p *ProviderProfile) UnmarshalJSON(data []byte) error {
	type plain ProviderProfile
	v := plain{RuntimeAdapter: "kagent", Enabled: true}
	if err := decodeStrict(data, &v); err != nil {
		return err
	}
	*p = ProviderProfile(v)
	return nil
}

func validProviderID(id string) bool { return len(id) <= 80 && providerIDPattern.MatchString(id) }

// Validate checks the profile against the PROVIDER_PROFILES rules. The registry uses it on the
// built-in Claude and Codex profiles, which are made from other settings.
func (p ProviderProfile) Validate() error { return p.validate() }

func (p ProviderProfile) validate() error {
	if !validProviderID(p.ID) {
		return errors.New("profile id must match ^[a-z][a-z0-9_-]*$ and be at most 80 characters")
	}
	if n := utf8.RuneCountInString(p.DisplayName); n < 1 || n > 200 {
		return fmt.Errorf("profile %q: display_name must be 1 to 200 characters", p.ID)
	}
	if p.RuntimeAdapter != "kagent" {
		return fmt.Errorf("profile %q: runtime_adapter must be kagent", p.ID)
	}
	if p.NativeProvider != NativeClaude && p.NativeProvider != NativeCodex {
		return fmt.Errorf("profile %q: native_provider must be claude or codex", p.ID)
	}
	if p.ConfigurationRevision == "" {
		return fmt.Errorf("profile %q: configuration_revision must not be empty", p.ID)
	}
	if len(p.Agents) == 0 {
		return fmt.Errorf("profile %q: at least one role AgentRef is required", p.ID)
	}
	for role, ref := range p.Agents {
		if !providerRoles[role] {
			return fmt.Errorf("profile %q: unknown agent role %q", p.ID, role)
		}
		for _, name := range []string{ref.Namespace, ref.Name} {
			if len(name) > 63 || !kagentNamePattern.MatchString(name) {
				return fmt.Errorf("profile %q: malformed AgentRef for role %q", p.ID, role)
			}
		}
	}
	aliases := map[string]bool{}
	for _, alias := range p.Aliases {
		if !validProviderID(alias) {
			return fmt.Errorf("profile %q: alias must match ^[a-z][a-z0-9_-]*$", p.ID)
		}
		if aliases[alias] {
			return fmt.Errorf("profile %q: aliases must be unique", p.ID)
		}
		aliases[alias] = true
	}
	caps := map[string]bool{}
	for _, c := range p.Capabilities {
		if err := c.validate(); err != nil {
			return fmt.Errorf("profile %q: %w", p.ID, err)
		}
		if caps[c.Capability] {
			return fmt.Errorf("profile %q: capability evidence must be unique by capability", p.ID)
		}
		caps[c.Capability] = true
	}
	return nil
}

// ProviderProfiles is the validated PROVIDER_PROFILES JSON list.
type ProviderProfiles []ProviderProfile

// UnmarshalText parses and validates the JSON list.
func (ps *ProviderProfiles) UnmarshalText(text []byte) error {
	var list []ProviderProfile
	if len(bytes.TrimSpace(text)) > 0 {
		if err := decodeStrict(text, &list); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, p := range list {
		if err := p.validate(); err != nil {
			return err
		}
		for _, name := range append([]string{p.ID}, p.Aliases...) {
			if seen[name] {
				return errors.New("duplicate provider ID or alias")
			}
			seen[name] = true
		}
	}
	for _, p := range list {
		for _, alias := range p.Aliases {
			if alias == NativeClaude || alias == NativeCodex {
				return errors.New("claude/codex are reserved legacy profile IDs")
			}
		}
		if (p.ID == NativeClaude || p.ID == NativeCodex) && p.NativeProvider != p.ID {
			return errors.New("legacy profile native provider must not change")
		}
	}
	*ps = list
	return nil
}

// Merge configuration limits.
const (
	MaxMergeConfigurationBytes = 128 * 1024
	MaxMergeConfigurations     = 100
	MergeToolName              = "merge_pull_request_with_approval"
	MergeOperation             = "mainloop.merge_pull_request_with_approval.v1"
)

// MergeConfiguration maps one AgentTemplate and provider to the protected merge tool.
type MergeConfiguration struct {
	TemplateName    string `json:"template_name"`
	Provider        string `json:"provider"`
	CompiledAlias   string `json:"compiled_alias"`
	Endpoint        string `json:"endpoint"`
	Tool            string `json:"tool"`
	RequireApproval bool   `json:"require_approval"`
	Operation       string `json:"operation"`
}

// UnmarshalJSON applies the field defaults before strict decoding.
func (m *MergeConfiguration) UnmarshalJSON(data []byte) error {
	type plain MergeConfiguration
	v := plain{Operation: MergeOperation}
	if err := decodeStrict(data, &v); err != nil {
		return err
	}
	*m = MergeConfiguration(v)
	return nil
}

// PublicName returns the provider-qualified tool name the agent sees.
func (m MergeConfiguration) PublicName() string {
	if m.Provider == NativeClaude {
		return "mcp__" + m.CompiledAlias + "__" + m.Tool
	}
	return m.CompiledAlias + "." + m.Tool
}

func (m MergeConfiguration) validate() error {
	if m.TemplateName == "" || m.CompiledAlias == "" || m.Endpoint == "" {
		return errors.New("template_name, compiled_alias and endpoint must not be empty")
	}
	if m.Provider != NativeClaude && m.Provider != NativeCodex {
		return errors.New("provider must be claude or codex")
	}
	if m.Tool != MergeToolName {
		return errors.New("tool must be " + MergeToolName)
	}
	if !m.RequireApproval {
		return errors.New("require_approval must be true")
	}
	if m.Operation != MergeOperation {
		return errors.New("operation must be " + MergeOperation)
	}
	return nil
}

// legacyMergeKeys mark the retired per-session merge configuration shape.
var legacyMergeKeys = []string{"owner_id", "binding_id", "runtime_session_id", "prepared_revision"}

// MergeConfigurations is the validated MAINLOOP_MERGE_CONFIGURATIONS JSON array.
type MergeConfigurations []MergeConfiguration

// UnmarshalText parses and validates the bounded JSON array. Errors never quote the value.
func (ms *MergeConfigurations) UnmarshalText(text []byte) error {
	if len(text) > MaxMergeConfigurationBytes {
		return errors.New("exceeds 128 KiB")
	}
	if len(bytes.TrimSpace(text)) == 0 {
		*ms = nil
		return nil
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(text, &raw); err != nil {
		return errors.New("must be a JSON array of objects")
	}
	for _, item := range raw {
		for _, key := range legacyMergeKeys {
			if _, ok := item[key]; ok {
				return errors.New("legacy per-session merge configuration is unsupported; use template_name and provider mappings")
			}
		}
	}
	if len(raw) > MaxMergeConfigurations {
		return errors.New("exceeds 100 entries")
	}
	var list []MergeConfiguration
	if err := decodeStrict(text, &list); err != nil {
		return errors.New("contains an invalid template mapping")
	}
	for _, m := range list {
		if err := m.validate(); err != nil {
			return errors.New("contains an invalid template mapping: " + err.Error())
		}
	}
	*ms = list
	return nil
}

// RegistryAllowlist is the ENVIRONMENT_REGISTRY_ALLOWLIST JSON list of OCI registries.
type RegistryAllowlist []string

// UnmarshalText parses the JSON list of registry host names.
func (r *RegistryAllowlist) UnmarshalText(text []byte) error {
	var list []string
	if err := decodeStrict(text, &list); err != nil {
		return err
	}
	for _, host := range list {
		if strings.TrimSpace(host) == "" {
			return errors.New("registry names must not be empty")
		}
	}
	*r = list
	return nil
}
