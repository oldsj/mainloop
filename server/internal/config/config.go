// Package config is the only place that reads the process environment. Every variable keeps the
// name the Python backend used, so the deployment manifests do not change.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/caarlos0/env/v11"
)

// Config holds every setting. Secret values use the Secret type and never print.
type Config struct {
	DB         Database
	Identity   Identity
	GitHub     GitHub
	Kagent     Kagent
	Tasks      Tasks
	Workspaces Workspaces
	Main       MainThread
	Git        Git
	Preview    Preview
	Health     Health
	Server     Server

	// EnvironmentRegistryAllowlist limits OCI registries for development environments.
	EnvironmentRegistryAllowlist RegistryAllowlist `env:"ENVIRONMENT_REGISTRY_ALLOWLIST" envDefault:"[\"ghcr.io\"]"`
	// WorkspaceDevelopmentPlatform is the deployment's workspace image platform.
	WorkspaceDevelopmentPlatform string `env:"WORKSPACE_DEVELOPMENT_PLATFORM" envDefault:"linux/arm64"`

	// Feature flags that were read straight from os.environ in the Python backend. Each is on
	// only for the exact value "true".
	OwnerHITLWritesEnabled   ExactTrue `env:"MAINLOOP_OWNER_HITL_WRITES_ENABLED"`
	OwnerPolicyWritesEnabled ExactTrue `env:"MAINLOOP_OWNER_POLICY_WRITES_ENABLED"`
	MergeToolsEnabled        ExactTrue `env:"MAINLOOP_MERGE_TOOLS_ENABLED"`

	// MergeConfigurations is the operator allowlist of protected merge tool mappings.
	MergeConfigurations MergeConfigurations `env:"MAINLOOP_MERGE_CONFIGURATIONS"`

	// Namespace is the Kubernetes namespace this instance runs in. In-cluster origins and the
	// prefix of per-binding credential Secrets derive from it.
	Namespace string `env:"MAINLOOP_NAMESPACE" envDefault:"mainloop"`
}

// Database is the PostgreSQL connection, built from parts.
type Database struct {
	Host     string `env:"DB_HOST" envDefault:"localhost"`
	Port     int    `env:"DB_PORT" envDefault:"5432"`
	Name     string `env:"DB_NAME" envDefault:"mainloop"`
	User     string `env:"DB_USER" envDefault:"mainloop"`
	Password Secret `env:"DB_PASSWORD"`
}

// Identity is the single owner and the HMAC key for agent tokens.
type Identity struct {
	OwnerID string `env:"MAINLOOP_OWNER_ID" envDefault:"local-dev-user"`
	// AgentTokenKey is the HMAC key for MCP and git capability tokens. It is required and has no
	// fallback to the database password.
	AgentTokenKey Secret `env:"AGENT_TOKEN_KEY,required,notEmpty"`
	// DevMode relaxes loopback Host and Origin checks.
	DevMode bool `env:"DEV_MODE"`
	// IsTestEnv has the same effect on those checks as DevMode.
	IsTestEnv bool `env:"IS_TEST_ENV"`
}

// GitHub App credentials. The private key is a single-line base64 PEM; the GitHub client
// validates it on first use.
type GitHub struct {
	AppID         string `env:"GITHUB_APP_ID"`
	AppPrivateKey Secret `env:"GITHUB_APP_PRIVATE_KEY"`
}

// Kagent is how Mainloop reaches the kagent controller and which agents it uses.
type Kagent struct {
	GatewayURL string `env:"KAGENT_GATEWAY_URL" envDefault:"http://kagent-controller.kagent.svc.cluster.local:8083"`
	// UserID is the fixed service identity kagent scopes sessions to.
	UserID string `env:"KAGENT_USER_ID" envDefault:"mainloop"`
	// ControlTokenFile is the path of a bearer token file, re-read on every request. The path is
	// validated at startup; the token is never held in settings.
	ControlTokenFile string `env:"KAGENT_CONTROL_TOKEN_FILE"`
	Namespace        string `env:"KAGENT_NAMESPACE" envDefault:"kagent"`
	// ActorAtespace is the Substrate atespace sessions run in (preview routing).
	ActorAtespace string `env:"KAGENT_ACTOR_ATESPACE" envDefault:"kagent"`

	MainAgent             string           `env:"KAGENT_MAIN_AGENT" envDefault:"mainloop-main"`
	ClaudeAgent           string           `env:"KAGENT_CLAUDE_AGENT" envDefault:"claude-subscription"`
	CodexAgent            string           `env:"KAGENT_CODEX_AGENT" envDefault:"codex-subscription-https"`
	WorkspaceClaudeAgent  string           `env:"KAGENT_WORKSPACE_CLAUDE_AGENT" envDefault:"claude-workspace"`
	WorkspaceCodexAgent   string           `env:"KAGENT_WORKSPACE_CODEX_AGENT" envDefault:"codex-workspace"`
	SupervisorClaudeAgent string           `env:"KAGENT_SUPERVISOR_CLAUDE_AGENT" envDefault:"claude-workspace"`
	SupervisorCodexAgent  string           `env:"KAGENT_SUPERVISOR_CODEX_AGENT" envDefault:"codex-workspace"`
	RequestTimeout        Seconds          `env:"KAGENT_REQUEST_TIMEOUT_SECONDS" envDefault:"30"`
	TurnTimeout           Seconds          `env:"KAGENT_TURN_TIMEOUT_SECONDS" envDefault:"1800"`
	SessionReadyTimeout   Seconds          `env:"KAGENT_SESSION_READY_TIMEOUT_SECONDS" envDefault:"120"`
	SendRetryBudget       Seconds          `env:"KAGENT_SEND_RETRY_BUDGET_SECONDS" envDefault:"30"`
	ProviderProfiles      ProviderProfiles `env:"PROVIDER_PROFILES"`
}

// Tasks limits delegated work.
type Tasks struct {
	DefaultProviderProfileID string  `env:"TASK_DEFAULT_PROVIDER_PROFILE_ID" envDefault:"claude"`
	MaxChildrenPerParent     int     `env:"TASK_MAX_CHILDREN_PER_PARENT" envDefault:"3"`
	MaxChildrenGlobal        int     `env:"TASK_MAX_CHILDREN_GLOBAL" envDefault:"6"`
	ReconcileInterval        Seconds `env:"TASK_RECONCILE_SECONDS" envDefault:"5"`
}

// Workspaces tunes workspace lifecycle loops.
type Workspaces struct {
	IdleCheckInterval  Seconds `env:"WORKSPACE_IDLE_CHECK_SECONDS" envDefault:"60"`
	CreateRetryInitial Seconds `env:"WORKSPACE_CREATE_RETRY_INITIAL_SECONDS" envDefault:"15"`
	CreateRetryMax     Seconds `env:"WORKSPACE_CREATE_RETRY_MAX_SECONDS" envDefault:"120"`
	CreateRetryWindow  Seconds `env:"WORKSPACE_CREATE_RETRY_WINDOW_SECONDS" envDefault:"600"`
}

// MainThread tunes the native main thread.
type MainThread struct {
	CarryOverMessages int    `env:"MAIN_CARRY_OVER_MESSAGES" envDefault:"6"`
	NativeChildKinds  string `env:"NATIVE_CHILD_KINDS" envDefault:"claude,codex"`
}

// Git configures the push gate and git transport.
type Git struct {
	PushGateEnabled         bool  `env:"PUSH_GATE_ENABLED"`
	TransportEnabled        bool  `env:"GIT_TRANSPORT_ENABLED"`
	PushGateMaxCommandBytes int64 `env:"PUSH_GATE_MAX_COMMAND_BYTES" envDefault:"262144"`
	PushGateMaxBodyBytes    int64 `env:"PUSH_GATE_MAX_BODY_BYTES" envDefault:"268435456"`
	// ReadOrigin and PushOrigin are derived from the namespace; any other value is rejected.
	ReadOrigin string `env:"GIT_READ_ORIGIN"`
	PushOrigin string `env:"GIT_PUSH_ORIGIN"`
}

// Preview configures the Substrate router used for workspace previews.
type Preview struct {
	RouterAddress  string  `env:"SUBSTRATE_ROUTER_ADDRESS" envDefault:"http://atenet-router.ate-system.svc.cluster.local:8081"`
	BaseURL        string  `env:"SUBSTRATE_PREVIEW_BASE_URL" envDefault:"http://localhost:8001"`
	ConnectTimeout Seconds `env:"SUBSTRATE_PREVIEW_CONNECT_TIMEOUT_SECONDS" envDefault:"5"`
}

// Health sets orchestration-health retention.
type Health struct {
	EventRetentionDays   int `env:"HEALTH_EVENT_RETENTION_DAYS" envDefault:"30"`
	SummaryRetentionDays int `env:"HEALTH_SUMMARY_RETENTION_DAYS" envDefault:"365"`
}

// Server is the owner-facing HTTP listener and its origin rules.
type Server struct {
	Host           string `env:"HOST" envDefault:"0.0.0.0"`
	Port           int    `env:"PORT" envDefault:"8000"`
	FrontendDomain string `env:"FRONTEND_DOMAIN" envDefault:"mainloop.example.com"`
	FrontendScheme string `env:"FRONTEND_SCHEME" envDefault:"https"`
	// APIDomain is the API's own domain when it differs from the frontend's.
	APIDomain string `env:"API_DOMAIN"`
	// APIHosts are extra Host names the API answers to (comma-separated, any port).
	APIHosts string `env:"MAINLOOP_API_HOSTS"`
}

// Load reads and validates the process environment.
func Load() (*Config, error) {
	return Parse(nil)
}

// Parse reads and validates the given variables. A nil map means the process environment.
func Parse(vars map[string]string) (*Config, error) {
	cfg := &Config{}
	opts := env.Options{Environment: vars}
	if vars == nil {
		opts.Environment = environ()
	}
	if err := env.ParseWithOptions(cfg, opts); err != nil {
		return nil, fmt.Errorf("config: %w", nameParseErrors(err, opts.Environment))
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

func environ() map[string]string {
	vars := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	return vars
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// validate checks cross-field rules and fills the derived origins.
func (c *Config) validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	c.Identity.OwnerID = strings.TrimSpace(c.Identity.OwnerID)
	if c.Identity.OwnerID == "" {
		fail("MAINLOOP_OWNER_ID must not be blank")
	}
	if strings.TrimSpace(c.Identity.AgentTokenKey.Reveal()) == "" {
		fail("AGENT_TOKEN_KEY must not be blank")
	}
	// The GitHub App is optional, but half of one is a mistake. The key itself is validated when
	// the client is built at startup.
	if hasID, hasKey := strings.TrimSpace(c.GitHub.AppID) != "", strings.TrimSpace(c.GitHub.AppPrivateKey.Reveal()) != ""; hasID != hasKey {
		fail("GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY must be set together")
	}
	c.GitHub.AppID = strings.TrimSpace(c.GitHub.AppID)
	if !dnsLabel.MatchString(c.Namespace) {
		fail("MAINLOOP_NAMESPACE must be a DNS label")
	} else if strings.Contains(c.Namespace, "-mcp") || strings.Contains(c.Namespace, "-git") {
		// Secret names are <ns>-mcp-* and <ns>-git-{read,push}-*; such a namespace could put its
		// names inside another instance's patterns.
		fail("MAINLOOP_NAMESPACE collides with credential Secret names")
	}
	for _, name := range []string{"read", "push"} {
		field, label := &c.Git.ReadOrigin, "GIT_READ_ORIGIN"
		if name == "push" {
			field, label = &c.Git.PushOrigin, "GIT_PUSH_ORIGIN"
		}
		want := c.ServiceOrigin("mainloop-git-" + name)
		switch *field {
		case "":
			*field = want
		case want:
		default:
			fail("%s is not sanctioned", label)
		}
	}
	if c.Server.FrontendScheme != "http" && c.Server.FrontendScheme != "https" {
		fail("FRONTEND_SCHEME must be http or https")
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		fail("PORT must be between 1 and 65535")
	}
	if c.DB.Port < 1 || c.DB.Port > 65535 {
		fail("DB_PORT must be between 1 and 65535")
	}
	if c.Tasks.MaxChildrenPerParent < 1 || c.Tasks.MaxChildrenGlobal < 1 {
		fail("TASK_MAX_CHILDREN_PER_PARENT and TASK_MAX_CHILDREN_GLOBAL must be at least 1")
	}
	if !validProviderID(c.Tasks.DefaultProviderProfileID) {
		fail("TASK_DEFAULT_PROVIDER_PROFILE_ID must match ^[a-z][a-z0-9_-]*$")
	}
	if c.Main.CarryOverMessages < 0 {
		fail("MAIN_CARRY_OVER_MESSAGES must not be negative")
	}
	if c.Git.PushGateMaxCommandBytes < 1 || c.Git.PushGateMaxBodyBytes < 1 {
		fail("PUSH_GATE_MAX_COMMAND_BYTES and PUSH_GATE_MAX_BODY_BYTES must be at least 1")
	}
	if c.Health.EventRetentionDays < 1 || c.Health.EventRetentionDays > 365 {
		fail("HEALTH_EVENT_RETENTION_DAYS must be between 1 and 365")
	}
	if c.Health.SummaryRetentionDays < 30 || c.Health.SummaryRetentionDays > 3650 {
		fail("HEALTH_SUMMARY_RETENTION_DAYS must be between 30 and 3650")
	}
	if c.Health.SummaryRetentionDays < c.Health.EventRetentionDays {
		fail("health summary retention must cover event retention")
	}
	if c.WorkspaceDevelopmentPlatform != "linux/arm64" && c.WorkspaceDevelopmentPlatform != "linux/amd64" {
		fail("WORKSPACE_DEVELOPMENT_PLATFORM must be linux/arm64 or linux/amd64")
	}
	errs = append(errs, c.Kagent.validate()...)
	return errors.Join(errs...)
}

// IsDev reports whether loopback Hosts and localhost Origins are accepted.
func (c *Config) IsDev() bool { return c.Identity.DevMode || c.Identity.IsTestEnv }

// ServiceOrigin returns the in-cluster origin of one of this instance's Services.
func (c *Config) ServiceOrigin(service string) string {
	return "http://" + service + "." + c.Namespace + ".svc.cluster.local"
}

// CredentialSecretPrefix is the prefix of per-binding Secret names in the kagent namespace.
func (c *Config) CredentialSecretPrefix() string { return c.Namespace }

// FrontendOrigin is the browser origin of the frontend.
func (c *Config) FrontendOrigin() string {
	return c.Server.FrontendScheme + "://" + c.Server.FrontendDomain
}

// ListenAddr is the owner API listen address.
func (c *Config) ListenAddr() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}

// DatabaseURL builds the connection string. It contains the password: pass it straight to the
// pool and never log it.
func (c *Config) DatabaseURL() string { return c.DB.URL() }

// URL builds the connection string. It contains the password: pass it straight to the pool and
// never log it.
func (d Database) URL() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(d.User, d.Password.Reveal()),
		Host:   fmt.Sprintf("%s:%d", d.Host, d.Port),
		Path:   "/" + d.Name,
	}
	return u.String()
}

// AllowedAPIHosts returns the lower-case host names the API serves (not preview hosts).
func (c *Config) AllowedAPIHosts() map[string]bool {
	names := []string{c.Server.FrontendDomain, c.Server.APIDomain}
	names = append(names, strings.Split(c.Server.APIHosts, ",")...)
	return hostSet(names)
}

// AllowedMCPHosts returns the lower-case host names the MCP listener serves besides loopback.
func (c *Config) AllowedMCPHosts() map[string]bool {
	return hostSet([]string{"mainloop-mcp." + c.Namespace + ".svc.cluster.local"})
}

// Hostname extracts the lower-case host name from a Host header value or a configured domain
// that may carry a port. It returns false for anything that is not a plain host[:port].
func Hostname(hostport string) (string, bool) {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" || strings.ContainsAny(hostport, "@/?#\\ \t") {
		return "", false
	}
	u, err := url.Parse("//" + hostport)
	if err != nil {
		return "", false
	}
	name := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return name, name != ""
}

func hostSet(names []string) map[string]bool {
	hosts := map[string]bool{}
	for _, name := range names {
		if h, ok := Hostname(name); ok {
			hosts[h] = true
		}
	}
	return hosts
}

const controlTokenMaxBytes = 4096

// checkControlTokenFile validates the token file's shape without keeping its contents or
// echoing them in an error.
func checkControlTokenFile(path string) error {
	f, err := os.Open(path) //nolint:gosec // operator-configured path
	if err != nil {
		return errors.New("KAGENT_CONTROL_TOKEN_FILE must name a readable token file")
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, controlTokenMaxBytes+1)
	n, _ := f.Read(buf)
	if n > controlTokenMaxBytes {
		return errors.New("KAGENT_CONTROL_TOKEN_FILE exceeds 4096 bytes")
	}
	token := strings.TrimSuffix(strings.TrimSuffix(string(buf[:n]), "\n"), "\r")
	if token == "" {
		return errors.New("KAGENT_CONTROL_TOKEN_FILE must contain one nonempty bearer")
	}
	for _, b := range []byte(token) {
		if b <= 32 || b >= 127 {
			return errors.New("KAGENT_CONTROL_TOKEN_FILE must contain one nonempty ASCII bearer")
		}
	}
	return nil
}
