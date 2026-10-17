package config

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureEnv reads testdata/<name>; a value "@file" is replaced by the contents of that file.
func fixtureEnv(t *testing.T, name string) map[string]string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	vars := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		if file, ok := strings.CutPrefix(v, "@"); ok {
			b, err := os.ReadFile(filepath.Join("testdata", file))
			if err != nil {
				t.Fatal(err)
			}
			v = string(b)
		}
		vars[k] = v
	}
	return vars
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProdShapedEnvironment(t *testing.T) {
	cfg, err := Parse(fixtureEnv(t, "prod.env"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DB.Host != "mainloop-pg-rw" || cfg.DB.Port != 5432 {
		t.Errorf("db = %+v", cfg.DB)
	}
	if got := cfg.DB.Password.Reveal(); got != "p@ss/word:with#odd chars" {
		t.Errorf("password lost: %q", got)
	}
	if !cfg.Git.TransportEnabled || !cfg.Git.PushGateEnabled || !bool(cfg.MergeToolsEnabled) || !bool(cfg.OwnerHITLWritesEnabled) {
		t.Errorf("flags not read: %+v", cfg)
	}
	if cfg.OwnerPolicyWritesEnabled {
		t.Error("policy writes must default off")
	}
	if len(cfg.Kagent.ProviderProfiles) != 2 || cfg.Kagent.ProviderProfiles[0].Agents["main"].Name != "mainloop-main" {
		t.Errorf("profiles = %+v", cfg.Kagent.ProviderProfiles)
	}
	claude, codex := cfg.Kagent.ProviderProfiles[0], cfg.Kagent.ProviderProfiles[1]
	if !claude.Enabled || claude.RuntimeAdapter != "kagent" || claude.Capabilities[1].Scope != "unverified" {
		t.Errorf("defaults not applied: %+v", claude)
	}
	if codex.Enabled {
		t.Error("enabled:false must be kept")
	}
	if len(cfg.MergeConfigurations) != 2 {
		t.Fatalf("merge configurations = %+v", cfg.MergeConfigurations)
	}
	if got := cfg.MergeConfigurations[0].PublicName(); got != "mcp__mainloop-merge-approval__merge_pull_request_with_approval" {
		t.Errorf("claude public name = %q", got)
	}
	if got := cfg.MergeConfigurations[1].PublicName(); got != "mainloop-merge-approval.merge_pull_request_with_approval" {
		t.Errorf("codex public name = %q", got)
	}
	if cfg.Git.ReadOrigin != "http://mainloop-git-read.mainloop.svc.cluster.local" {
		t.Errorf("git read origin = %q", cfg.Git.ReadOrigin)
	}
	if cfg.FrontendOrigin() != "https://mainloop.example.ts.net" {
		t.Errorf("frontend origin = %q", cfg.FrontendOrigin())
	}
	if !cfg.AllowedAPIHosts()["mainloop-backend"] || !cfg.AllowedAPIHosts()["mainloop.example.ts.net"] {
		t.Errorf("api hosts = %v", cfg.AllowedAPIHosts())
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr() != "0.0.0.0:8000" || cfg.Identity.OwnerID != "local-dev-user" {
		t.Errorf("defaults: %s %s", cfg.ListenAddr(), cfg.Identity.OwnerID)
	}
	if got := cfg.EnvironmentRegistryAllowlist; len(got) != 1 || got[0] != "ghcr.io" {
		t.Errorf("registry allowlist = %v", got)
	}
	if cfg.Kagent.RequestTimeout.Duration() != 30*time.Second || cfg.Workspaces.CreateRetryWindow.Duration() != 600*time.Second {
		t.Errorf("durations: %v %v", cfg.Kagent.RequestTimeout, cfg.Workspaces.CreateRetryWindow)
	}
	if cfg.Tasks.ProvisionWindow.Duration() != 900*time.Second || cfg.Tasks.ProjectionEvery.Duration() != 60*time.Second {
		t.Errorf("task provisioning defaults: %+v", cfg.Tasks)
	}
	if cfg.Tasks.ReconcileInterval.Duration() != 5*time.Second || cfg.WorkspaceDevelopmentPlatform != "linux/arm64" || cfg.EnvironmentFollowInterval.Duration() != time.Hour {
		t.Errorf("tasks/platform defaults: %+v", cfg.Tasks)
	}
	if cfg.Events.RetentionHours != 24 || cfg.Events.SubscriberBuffer != 64 || cfg.Events.PollInterval.Duration() != 5*time.Second {
		t.Errorf("events defaults: %+v", cfg.Events)
	}
	if cfg.MergeToolsEnabled || cfg.MergeConfigurations != nil || cfg.Kagent.ProviderProfiles != nil {
		t.Error("merge and provider defaults must be empty")
	}
	if cfg.IsDev() {
		t.Error("dev must default off")
	}
	if !cfg.AllowedMCPHosts()["mainloop-mcp.mainloop.svc.cluster.local"] {
		t.Errorf("mcp hosts = %v", cfg.AllowedMCPHosts())
	}
	want := "postgres://mainloop:@localhost:5432/mainloop"
	if got := cfg.DatabaseURL(); got != want {
		t.Errorf("database url = %q", got)
	}
}

func TestPushBodyFitsScratchBudget(t *testing.T) {
	for _, tc := range []struct {
		bytes int64
		valid bool
	}{{1, true}, {PushGateDiskBytes, true}, {PushGateDiskBytes + 1, false}} {
		_, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "PUSH_GATE_MAX_BODY_BYTES": fmt.Sprint(tc.bytes)})
		if (err == nil) != tc.valid {
			t.Fatalf("body bytes %d: %v", tc.bytes, err)
		}
	}
}

func TestAgentTokenKeyIsRequired(t *testing.T) {
	// There is no fallback to the database password, even in dev.
	for _, vars := range []map[string]string{
		{},
		{"AGENT_TOKEN_KEY": ""},
		{"AGENT_TOKEN_KEY": " \t\n"},
		{"DB_PASSWORD": "db-secret", "DEV_MODE": "true"},
		{"DB_PASSWORD": "db-secret", "IS_TEST_ENV": "true"},
	} {
		_, err := Parse(vars)
		if err == nil || !strings.Contains(err.Error(), "AGENT_TOKEN_KEY") {
			t.Errorf("Parse(%v) err = %v, want AGENT_TOKEN_KEY error", keys(vars), err)
		}
		if err != nil && strings.Contains(err.Error(), "db-secret") {
			t.Errorf("error leaks the password: %v", err)
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestInvalidValues(t *testing.T) {
	base := func(k, v string) map[string]string { return map[string]string{"AGENT_TOKEN_KEY": "k", k: v} }
	merge := fixture(t, "merge_configurations.json")
	profiles := fixture(t, "provider_profiles.json")
	tests := []struct {
		name string
		vars map[string]string
		want string
	}{
		{"blank owner", base("MAINLOOP_OWNER_ID", "  "), "MAINLOOP_OWNER_ID"},
		{"bad mcp host", base("MAINLOOP_MCP_HOSTS", "ok.example,not a host"), "MAINLOOP_MCP_HOSTS"},
		{"bad namespace", base("MAINLOOP_NAMESPACE", "Not_A_Label"), "MAINLOOP_NAMESPACE"},
		{"colliding namespace", base("MAINLOOP_NAMESPACE", "team-git"), "collides"},
		{"foreign git origin", base("GIT_READ_ORIGIN", "http://evil.example"), "GIT_READ_ORIGIN"},
		{"bad scheme", base("FRONTEND_SCHEME", "ftp"), "FRONTEND_SCHEME"},
		{"bad port", base("PORT", "70000"), "PORT"},
		{"non-numeric port", base("DB_PORT", "abc"), "DB_PORT"},
		{"zero children", base("TASK_MAX_CHILDREN_PER_PARENT", "0"), "TASK_MAX_CHILDREN"},
		{"bad seconds", base("TASK_RECONCILE_SECONDS", "0"), "TASK_RECONCILE_SECONDS"},
		{"zero provision window", base("TASK_PROVISION_WINDOW_SECONDS", "0"), "TASK_PROVISION_WINDOW_SECONDS"},
		{"negative projection period", base("TASK_PROJECTION_SECONDS", "-5"), "TASK_PROJECTION_SECONDS"},
		{"zero stall timeout", base("TASK_STALL_TIMEOUT_SECONDS", "0"), "TASK_STALL_TIMEOUT_SECONDS"},
		{"zero max runtime", base("TASK_MAX_RUNTIME_SECONDS", "0"), "TASK_MAX_RUNTIME_SECONDS"},
		{"runtime bound below stall timeout", map[string]string{"AGENT_TOKEN_KEY": "k", "TASK_STALL_TIMEOUT_SECONDS": "100", "TASK_MAX_RUNTIME_SECONDS": "50"}, "TASK_MAX_RUNTIME_SECONDS"},
		{"negative seconds", base("KAGENT_REQUEST_TIMEOUT_SECONDS", "-1"), "KAGENT_REQUEST_TIMEOUT_SECONDS"},
		{"nan seconds", base("TASK_RECONCILE_SECONDS", "NaN"), "TASK_RECONCILE_SECONDS"},
		{"inf seconds", base("TASK_RECONCILE_SECONDS", "Inf"), "TASK_RECONCILE_SECONDS"},
		{"retention order", map[string]string{"AGENT_TOKEN_KEY": "k", "HEALTH_EVENT_RETENTION_DAYS": "200", "HEALTH_SUMMARY_RETENTION_DAYS": "100"}, "retention"},
		{"event retention range", base("HEALTH_EVENT_RETENTION_DAYS", "0"), "HEALTH_EVENT_RETENTION_DAYS"},
		{"follow interval too short", base("ENVIRONMENT_FOLLOW_MIN_INTERVAL_SECONDS", "59"), "ENVIRONMENT_FOLLOW_MIN_INTERVAL_SECONDS"},
		{"follow interval too long", base("ENVIRONMENT_FOLLOW_MIN_INTERVAL_SECONDS", "604801"), "ENVIRONMENT_FOLLOW_MIN_INTERVAL_SECONDS"},
		{"platform", base("WORKSPACE_DEVELOPMENT_PLATFORM", "windows/amd64"), "WORKSPACE_DEVELOPMENT_PLATFORM"},
		{"task profile id", base("TASK_DEFAULT_PROVIDER_PROFILE_ID", "Bad ID"), "TASK_DEFAULT_PROVIDER_PROFILE_ID"},
		{"missing control token file", base("KAGENT_CONTROL_TOKEN_FILE", "/nonexistent/token"), "readable token file"},
		{"control token with other user", map[string]string{"AGENT_TOKEN_KEY": "k", "KAGENT_CONTROL_TOKEN_FILE": "/nonexistent", "KAGENT_USER_ID": "someone"}, "KAGENT_USER_ID=mainloop"},
		{"registry allowlist not json", base("ENVIRONMENT_REGISTRY_ALLOWLIST", "ghcr.io"), "ENVIRONMENT_REGISTRY_ALLOWLIST"},
		{"registry allowlist blank entry", base("ENVIRONMENT_REGISTRY_ALLOWLIST", `["ghcr.io"," "]`), "ENVIRONMENT_REGISTRY_ALLOWLIST"},
		{"profiles not an array", base("PROVIDER_PROFILES", `{"id":"x"}`), "PROVIDER_PROFILES"},
		{"profiles trailing data", base("PROVIDER_PROFILES", `[] []`), "PROVIDER_PROFILES"},
		{"profile unknown field", base("PROVIDER_PROFILES", strings.Replace(profiles, `"enabled": false`, `"enabled": false, "surprise": 1`, 1)), "surprise"},
		{"profile bad provider", base("PROVIDER_PROFILES", strings.Replace(profiles, `"native_provider": "codex"`, `"native_provider": "gemini"`, 1)), "native_provider"},
		{"profile no agents", base("PROVIDER_PROFILES", `[{"id":"x","display_name":"X","native_provider":"claude","configuration_revision":"r","agents":{}}]`), "AgentRef"},
		{"profile unknown role", base("PROVIDER_PROFILES", `[{"id":"x","display_name":"X","native_provider":"claude","configuration_revision":"r","agents":{"boss":{"namespace":"kagent","name":"a"}}}]`), "unknown agent role"},
		{"profile bad agent name", base("PROVIDER_PROFILES", `[{"id":"x","display_name":"X","native_provider":"claude","configuration_revision":"r","agents":{"main":{"namespace":"kagent","name":"Not Valid"}}}]`), "malformed AgentRef"},
		{"profile wrong type", base("PROVIDER_PROFILES", `[{"id":5}]`), "must be a"},
		{"profile duplicate id", base("PROVIDER_PROFILES", `[`+profileJSON("a", "claude", "")+`,`+profileJSON("a", "claude", "")+`]`), "duplicate"},
		{"profile alias collides with id", base("PROVIDER_PROFILES", `[`+profileJSON("a", "claude", "")+`,`+profileJSON("b", "claude", `"a"`)+`]`), "duplicate"},
		{"reserved alias", base("PROVIDER_PROFILES", `[`+profileJSON("a", "claude", `"codex"`)+`]`), "reserved"},
		{"reserved id redirected", base("PROVIDER_PROFILES", `[`+profileJSON("codex", "claude", "")+`]`), "must not change"},
		{"proved without evidence", base("PROVIDER_PROFILES", strings.Replace(profiles, `"state": "unknown"`, `"state": "proved"`, 1)), "scoped evidence"},
		{"merge not an array", base("MAINLOOP_MERGE_CONFIGURATIONS", `{}`), "MAINLOOP_MERGE_CONFIGURATIONS"},
		{"merge legacy shape", base("MAINLOOP_MERGE_CONFIGURATIONS", fixture(t, "merge_configurations_legacy.json")), "legacy per-session"},
		{"merge unknown field", base("MAINLOOP_MERGE_CONFIGURATIONS", strings.Replace(merge, `"tool"`, `"extra": 1, "tool"`, 1)), "invalid template mapping"},
		{"merge approval off", base("MAINLOOP_MERGE_CONFIGURATIONS", strings.Replace(merge, `"require_approval": true`, `"require_approval": false`, 1)), "require_approval"},
		{"merge other tool", base("MAINLOOP_MERGE_CONFIGURATIONS", strings.Replace(merge, `"merge_pull_request_with_approval"`, `"delete_repo"`, 1)), "tool must be"},
		{"merge other provider", base("MAINLOOP_MERGE_CONFIGURATIONS", strings.Replace(merge, `"provider": "claude"`, `"provider": "gemini"`, 1)), "provider"},
		{"merge too many", base("MAINLOOP_MERGE_CONFIGURATIONS", "["+strings.Repeat(`{},`, 100)+`{}]`), "exceeds 100"},
		{"merge too large", base("MAINLOOP_MERGE_CONFIGURATIONS", "["+strings.Repeat(" ", MaxMergeConfigurationBytes)+"]"), "128 KiB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.vars)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func profileJSON(id, provider, aliases string) string {
	return fmt.Sprintf(`{"id":%q,"display_name":"P","native_provider":%q,"configuration_revision":"r","agents":{"main":{"namespace":"kagent","name":"a"}},"aliases":[%s]}`, id, provider, aliases)
}

func TestEmptyJSONValuesUseDefaults(t *testing.T) {
	cfg, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "PROVIDER_PROFILES": "", "MAINLOOP_MERGE_CONFIGURATIONS": "  "})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kagent.ProviderProfiles != nil || cfg.MergeConfigurations != nil {
		t.Error("blank JSON values must mean empty")
	}
}

func TestSecretsNeverPrint(t *testing.T) {
	const (
		dbPass = "db-pass-sentinel"
		key    = "hmac-key-sentinel"
		pem    = "pem-sentinel"
	)
	cfg, err := Parse(map[string]string{"AGENT_TOKEN_KEY": key, "DB_PASSWORD": dbPass, "GITHUB_APP_ID": "123", "GITHUB_APP_PRIVATE_KEY": pem})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	logger.Info("config", "db", cfg.DB, "identity", cfg.Identity, "github", cfg.GitHub, "all", cfg)
	printed := fmt.Sprintf("%v|%+v|%#v|%s|%v", cfg, cfg, cfg, cfg.DB.Password, cfg.GitHub)
	for _, s := range []string{logs.String(), printed} {
		for _, secret := range []string{dbPass, key, pem} {
			if strings.Contains(s, secret) {
				t.Errorf("secret %q appears in output %.80q", secret, s)
			}
		}
	}
	if !strings.Contains(logs.String(), "[redacted]") {
		t.Errorf("redaction marker missing from logs: %s", logs.String())
	}
}

func TestParseErrorsNeverQuoteSecretBearingJSON(t *testing.T) {
	const marker = "super-secret-marker"
	for _, name := range []string{"PROVIDER_PROFILES", "MAINLOOP_MERGE_CONFIGURATIONS"} {
		for _, value := range []string{
			`[{"id":"` + marker + `","bogus":"` + marker + `"}]`,
			`[{"id":"` + marker + `"`,
			`{"` + marker + `": 1}`,
			`not json ` + marker,
			`[{"owner_id":"` + marker + `"}]`,
		} {
			_, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k", name: value})
			if err == nil {
				t.Fatalf("%s=%q: expected an error", name, value)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("%s error quotes the value: %v", name, err)
			}
		}
	}
}

func TestControlTokenFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name, content, want string
	}{
		{"ok", "tok_abc123\n", ""},
		{"empty", "", "nonempty"},
		{"space inside", "tok abc", "ASCII bearer"},
		{"non ascii", "toké", "ASCII bearer"},
		{"too long", strings.Repeat("a", 4097), "exceeds 4096"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "KAGENT_CONTROL_TOKEN_FILE": write(tt.name, tt.content)})
			switch {
			case tt.want == "" && err != nil:
				t.Fatal(err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), tt.content) && tt.content != "" {
				t.Error("error quotes the token")
			}
		})
	}
}

// Cases mined from backend/tests/runtime/test_host_guard.py (test_api_hosts_are_normalised,
// test_configured_domains_with_ports_are_compared_as_hostnames).
func TestAllowedAPIHostsAreNormalised(t *testing.T) {
	cfg, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "A.example", "MAINLOOP_API_HOSTS": " B.example. ,,c"})
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.AllowedAPIHosts()
	if len(got) != 3 || !got["a.example"] || !got["b.example"] || !got["c"] {
		t.Errorf("hosts = %v", got)
	}

	cfg, err = Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "localhost:5173", "API_DOMAIN": "API.example:8443"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FrontendOrigin() != "https://localhost:5173" {
		t.Errorf("frontend origin = %q", cfg.FrontendOrigin())
	}
	if !cfg.AllowedAPIHosts()["api.example"] || !cfg.AllowedAPIHosts()["localhost"] {
		t.Errorf("hosts = %v", cfg.AllowedAPIHosts())
	}
}

func TestHostname(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"mainloop.example.ts.net", "mainloop.example.ts.net", true},
		{"MAINLOOP.EXAMPLE.TS.NET:8443", "mainloop.example.ts.net", true},
		{"b.example.", "b.example", true},
		{"[::1]:8000", "::1", true},
		{"127.0.0.1:8000", "127.0.0.1", true},
		{"", "", false},
		{"[bad", "", false},
		{"user@evil.example", "", false},
		{"evil.example/path", "", false},
		{"a b", "", false},
		{"host:notaport", "", false},
	}
	for _, tt := range tests {
		got, ok := Hostname(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("Hostname(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestDisplayNameCountsRunes(t *testing.T) {
	// 150 three-byte runes: over 200 bytes, within 200 characters.
	name := strings.Repeat("日", 150)
	profile := func(name string) string {
		return `[{"id":"x","display_name":"` + name + `","native_provider":"claude","configuration_revision":"r","agents":{"main":{"namespace":"kagent","name":"a"}}}]`
	}
	if _, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "PROVIDER_PROFILES": profile(name)}); err != nil {
		t.Fatalf("150 runes rejected: %v", err)
	}
	if _, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "PROVIDER_PROFILES": profile(strings.Repeat("日", 201))}); err == nil {
		t.Fatal("201 runes accepted")
	}
}

func TestMigrateAllowResetIsExactlyOne(t *testing.T) {
	for value, want := range map[string]bool{"": false, "1": true, "0": false, "true": false, "t": false, "T": false, "yes": false} {
		t.Setenv("MAINLOOP_ALLOW_RESET", value)
		m, err := LoadMigrate()
		if err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if m.AllowReset != want {
			t.Errorf("MAINLOOP_ALLOW_RESET=%q: AllowReset = %v, want %v", value, m.AllowReset, want)
		}
	}
}

func TestParseKagentNeedsNothingElse(t *testing.T) {
	k, err := ParseKagent(map[string]string{"KAGENT_GATEWAY_URL": "http://127.0.0.1:18083"})
	if err != nil {
		t.Fatal(err)
	}
	if k.GatewayURL != "http://127.0.0.1:18083" || k.UserID != "mainloop" || k.MainAgent != "mainloop-main" {
		t.Fatalf("got %+v", k)
	}
}

func TestParseKagentChecksTheTokenFile(t *testing.T) {
	_, err := ParseKagent(map[string]string{"KAGENT_CONTROL_TOKEN_FILE": "/nonexistent/token"})
	if err == nil || !strings.Contains(err.Error(), "KAGENT_CONTROL_TOKEN_FILE") {
		t.Fatalf("got %v", err)
	}
	_, err = ParseKagent(map[string]string{"KAGENT_REQUEST_TIMEOUT_SECONDS": "soon"})
	if err == nil || !strings.Contains(err.Error(), "KAGENT_REQUEST_TIMEOUT_SECONDS") {
		t.Fatalf("got %v", err)
	}
}

func TestGitHubAppSettingsComeTogether(t *testing.T) {
	base := map[string]string{"AGENT_TOKEN_KEY": "k"}
	with := func(extra map[string]string) error {
		vars := map[string]string{}
		for k, v := range base {
			vars[k] = v
		}
		for k, v := range extra {
			vars[k] = v
		}
		_, err := Parse(vars)
		return err
	}
	if err := with(nil); err != nil {
		t.Fatalf("neither set: %v", err)
	}
	if err := with(map[string]string{"GITHUB_APP_ID": "1", "GITHUB_APP_PRIVATE_KEY": "x"}); err != nil {
		t.Fatalf("both set: %v", err)
	}
	for name, extra := range map[string]map[string]string{
		"id only":  {"GITHUB_APP_ID": "1"},
		"key only": {"GITHUB_APP_PRIVATE_KEY": "x"},
		"blank id": {"GITHUB_APP_ID": "  ", "GITHUB_APP_PRIVATE_KEY": "x"},
	} {
		err := with(extra)
		if err == nil || strings.Contains(err.Error(), "x\n") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestGatesAreOnlyForTheExactValueTrue(t *testing.T) {
	gates := map[string]func(*Config) bool{
		"MAINLOOP_OWNER_POLICY_WRITES_ENABLED": func(c *Config) bool { return bool(c.OwnerPolicyWritesEnabled) },
		"MAINLOOP_OWNER_HITL_WRITES_ENABLED":   func(c *Config) bool { return bool(c.OwnerHITLWritesEnabled) },
		"MAINLOOP_MERGE_TOOLS_ENABLED":         func(c *Config) bool { return bool(c.MergeToolsEnabled) },
		"MAINLOOP_MERGE_PROTECTED_AUTO":        func(c *Config) bool { return bool(c.MergeProtectedAuto) },
	}
	for name, on := range gates {
		for value, want := range map[string]bool{
			"true": true, "TRUE": false, "True": false, "1": false, "t": false, "T": false, "yes": false,
			"false": false, "": false, " true": false, "true ": false, "junk": false,
		} {
			cfg, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k", name: value})
			if err != nil {
				t.Fatalf("%s=%q must not fail startup: %v", name, value, err)
			}
			if got := on(cfg); got != want {
				t.Errorf("%s=%q: enabled = %v, want %v", name, value, got, want)
			}
		}
		cfg, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k"})
		if err != nil || on(cfg) {
			t.Errorf("%s unset: enabled = %v, err %v", name, on(cfg), err)
		}
	}
}

func TestEventsSettingsAreValidated(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"retention zero":     {"MAINLOOP_EVENTS_RETENTION_HOURS": "0"},
		"retention too long": {"MAINLOOP_EVENTS_RETENTION_HOURS": "721"},
		"buffer too small":   {"MAINLOOP_EVENTS_SUBSCRIBER_BUFFER": "1"},
		"buffer too large":   {"MAINLOOP_EVENTS_SUBSCRIBER_BUFFER": "100000"},
		"not a number":       {"MAINLOOP_EVENTS_RETENTION_HOURS": "a day"},
	} {
		t.Run(name, func(t *testing.T) {
			vars["AGENT_TOKEN_KEY"] = "k"
			if _, err := Parse(vars); err == nil {
				t.Error("expected an error")
			}
		})
	}
	cfg, err := Parse(map[string]string{
		"AGENT_TOKEN_KEY": "k", "MAINLOOP_EVENTS_RETENTION_HOURS": "48", "MAINLOOP_EVENTS_SUBSCRIBER_BUFFER": "128", "MAINLOOP_EVENTS_POLL_SECONDS": "2",
	})
	if err != nil || cfg.Events.RetentionHours != 48 || cfg.Events.SubscriberBuffer != 128 || cfg.Events.PollInterval.Duration() != 2*time.Second {
		t.Errorf("custom events settings: %+v, %v", cfg.Events, err)
	}
}

func TestAllowedMCPHosts(t *testing.T) {
	cfg, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "MAINLOOP_NAMESPACE": "team"})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.AllowedMCPHosts(); len(got) != 1 || !got["mainloop-mcp.team.svc.cluster.local"] {
		t.Errorf("default hosts = %v", got)
	}
	cfg, err = Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "MAINLOOP_MCP_HOSTS": " MCP.example:8002 ,,b.example. "})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.AllowedMCPHosts(); len(got) != 2 || !got["mcp.example"] || !got["b.example"] {
		t.Errorf("configured hosts = %v", got)
	}
}

func TestInCluster(t *testing.T) {
	cfg, _ := Parse(map[string]string{"AGENT_TOKEN_KEY": "k"})
	if cfg.InCluster() {
		t.Error("InCluster without KUBERNETES_SERVICE_HOST")
	}
	cfg, _ = Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "KUBERNETES_SERVICE_HOST": "10.0.0.1"})
	if !cfg.InCluster() {
		t.Error("not InCluster with KUBERNETES_SERVICE_HOST")
	}
}

func TestMCPOrigin(t *testing.T) {
	cfg, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "MAINLOOP_NAMESPACE": "team"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.MCPOrigin(), "http://mainloop-mcp.team.svc.cluster.local"; got != want {
		t.Errorf("MCPOrigin = %q, want %q", got, want)
	}
	// The origin's host is one the MCP listener accepts by default.
	if !cfg.AllowedMCPHosts()["mainloop-mcp.team.svc.cluster.local"] {
		t.Error("the origin's host is not served")
	}
}

func TestMCPHostsMustServeTheMCPOriginInACluster(t *testing.T) {
	base := map[string]string{"AGENT_TOKEN_KEY": "k", "MAINLOOP_NAMESPACE": "team", "KUBERNETES_SERVICE_HOST": "10.0.0.1"}
	with := func(hosts string) map[string]string {
		m := map[string]string{"MAINLOOP_MCP_HOSTS": hosts}
		for k, v := range base {
			m[k] = v
		}
		return m
	}
	if _, err := Parse(with("mcp.example")); err == nil || !strings.Contains(err.Error(), "MAINLOOP_MCP_HOSTS must include") {
		t.Errorf("a list without the origin's host: err = %v", err)
	}
	if _, err := Parse(with("mcp.example, mainloop-mcp.team.svc.cluster.local")); err != nil {
		t.Errorf("a list with the origin's host: %v", err)
	}
	if _, err := Parse(base); err != nil {
		t.Errorf("the default list: %v", err)
	}
	// Outside a cluster no credential is issued, so the origin is never handed out.
	if _, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "MAINLOOP_NAMESPACE": "team", "MAINLOOP_MCP_HOSTS": "mcp.example"}); err != nil {
		t.Errorf("outside a cluster: %v", err)
	}
}

func TestMainProviderAndItsCodexAgentHaveDefaults(t *testing.T) {
	cfg, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Main.Provider != "claude" || cfg.Kagent.MainAgent != "mainloop-main" || cfg.Kagent.MainCodexAgent != "mainloop-main-codex" {
		t.Fatalf("defaults: provider %q, agents %q %q", cfg.Main.Provider, cfg.Kagent.MainAgent, cfg.Kagent.MainCodexAgent)
	}
	cfg, err = Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "MAIN_PROVIDER": "codex", "KAGENT_MAIN_CODEX_AGENT": "other-main"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Main.Provider != "codex" || cfg.Kagent.MainCodexAgent != "other-main" {
		t.Fatalf("overrides: provider %q, agent %q", cfg.Main.Provider, cfg.Kagent.MainCodexAgent)
	}
	if _, err := Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "MAIN_PROVIDER": "gemini"}); err == nil || !strings.Contains(err.Error(), "MAIN_PROVIDER") {
		t.Fatalf("an unknown provider was accepted: %v", err)
	}
}
