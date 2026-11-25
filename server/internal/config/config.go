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
	"time"

	"github.com/caarlos0/env/v11"
)

// Config holds every setting. Secret values use the Secret type and never print.
type Config struct {
	DB          Database
	Identity    Identity
	GitHub      GitHub
	Kagent      Kagent
	Tasks       Tasks
	Workspaces  Workspaces
	Main        MainThread
	Git         Git
	Preview     Preview
	Health      Health
	Transcripts Transcripts
	Events      Events
	Sessions    Sessions
	Server      Server
	Ntfy        Ntfy
	APNs        APNs
	Auth        Auth

	// EnvironmentRegistryAllowlist limits OCI registries for development environments.
	EnvironmentRegistryAllowlist RegistryAllowlist `env:"ENVIRONMENT_REGISTRY_ALLOWLIST" envDefault:"[\"ghcr.io\"]"`
	// EnvironmentFollowInterval is the least time between two images a following environment
	// accepts from its watched tag. The first image is always accepted.
	EnvironmentFollowInterval Seconds `env:"ENVIRONMENT_FOLLOW_MIN_INTERVAL_SECONDS" envDefault:"3600"`
	// WorkspaceDevelopmentPlatform is the deployment's workspace image platform.
	WorkspaceDevelopmentPlatform string `env:"WORKSPACE_DEVELOPMENT_PLATFORM" envDefault:"linux/arm64"`

	// Feature flags that were read straight from os.environ in the Python backend. Each is on
	// only for the exact value "true".
	OwnerHITLWritesEnabled   ExactTrue `env:"MAINLOOP_OWNER_HITL_WRITES_ENABLED"`
	OwnerPolicyWritesEnabled ExactTrue `env:"MAINLOOP_OWNER_POLICY_WRITES_ENABLED"`
	MergeToolsEnabled        ExactTrue `env:"MAINLOOP_MERGE_TOOLS_ENABLED"`
	// MergeProtectedAuto lets a protected-path match merge on green without an owner card (still
	// recorded in the frozen facts and route for audit), and makes evidence Prepare could not read
	// a refusal instead of an approval card nobody can safely approve. The project's own approval
	// policy is unaffected.
	MergeProtectedAuto ExactTrue `env:"MAINLOOP_MERGE_PROTECTED_AUTO"`
	// DevloopEnabled turns on the dev_* MCP tools (live sync to the shared mainloop-dev instance).
	// Off by default; see docs/specs/devloop.md.
	DevloopEnabled ExactTrue `env:"MAINLOOP_DEVLOOP_ENABLED"`
	// PrivilegedActionsEnabled turns on privileged actions (docs/specs/privileged-actions.md): the
	// request tools, the approval cards and the executor. serve-api refuses to start with it on
	// unless MAINLOOP_AUTH_MODE is enforce, since an approval must be attributable to the owner.
	PrivilegedActionsEnabled ExactTrue `env:"MAINLOOP_PRIVILEGED_ACTIONS_ENABLED"`
	// PrivilegedExecutorEnabled says a serve-executor is deployed beside this Mainloop, so the
	// executor types (onepassword.*) are accepted. Without it they answer unavailable.
	PrivilegedExecutorEnabled ExactTrue `env:"MAINLOOP_PRIVILEGED_EXECUTOR_ENABLED"`
	// PrivilegedKubernetesEnabled says that executor may mint Kubernetes credentials, so the k8s.*
	// types are accepted.
	PrivilegedKubernetesEnabled ExactTrue `env:"MAINLOOP_PRIVILEGED_EXECUTOR_KUBERNETES_ENABLED"`
	// PrivilegedOmniEnabled accepts command.omnictl and command.talosctl: the executor has Omni
	// access (executor.omni in the chart).
	PrivilegedOmniEnabled ExactTrue `env:"MAINLOOP_PRIVILEGED_EXECUTOR_OMNI_ENABLED"`
	// PrivilegedApprovalKeys is the executor's pinned approval keys (the same value as
	// serve-executor's). With any pinned, serve-api accepts an approval of an executor type only
	// from a phone whose key is pinned, signed with it.
	PrivilegedApprovalKeys string `env:"MAINLOOP_PRIVILEGED_APPROVAL_KEYS"`
	// PrivilegedApprovalKeysRequired refuses to start without a pinned approval key, and takes an
	// executor type's approval only with a pinned key's signature.
	PrivilegedApprovalKeysRequired ExactTrue `env:"MAINLOOP_PRIVILEGED_APPROVAL_KEYS_REQUIRED"`
	// PrivilegedSSHEnabled accepts command.ssh: the executor reaches home-network hosts over ssh
	// (executor.ssh in the chart).
	PrivilegedSSHEnabled ExactTrue `env:"MAINLOOP_PRIVILEGED_EXECUTOR_SSH_ENABLED"`
	// PrivilegedUniFiEnabled accepts http.unifi: the executor reaches the UniFi console
	// (executor.unifi in the chart).
	PrivilegedUniFiEnabled ExactTrue `env:"MAINLOOP_PRIVILEGED_EXECUTOR_UNIFI_ENABLED"`

	// WebFetchPerMinute bounds the open-web web_fetch tool's fetches per owner per minute, and
	// WebFetchDenyCIDRs adds comma-separated ranges it never connects to, beyond the built-in
	// non-public ones. The tool is on only for agents the owner opted in (docs/specs/web-fetch.md).
	WebFetchPerMinute int    `env:"MAINLOOP_WEB_FETCH_PER_MINUTE" envDefault:"30"`
	WebFetchDenyCIDRs string `env:"MAINLOOP_WEB_FETCH_DENY_CIDRS"`

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

	MainAgent string `env:"KAGENT_MAIN_AGENT" envDefault:"mainloop-main"`
	// MainCodexAgent runs the main thread when the owner selects Codex; MainAgent runs it for Claude.
	MainCodexAgent        string           `env:"KAGENT_MAIN_CODEX_AGENT" envDefault:"mainloop-main-codex"`
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

	// WorkspaceClaudeOverflowAgent and WorkspaceCodexOverflowAgent are the built-in profiles'
	// agent-overflow Agents: a code child task admitted in the overflow capacity tier
	// (TASK_MAX_CHILDREN_OVERFLOW) runs on it instead of the workspace Agent, so Substrate places
	// it on the overflow worker pool, and keeps it for that attempt's life. Empty, the default, is
	// none: such a task runs on the workspace Agent.
	WorkspaceClaudeOverflowAgent string `env:"KAGENT_WORKSPACE_CLAUDE_OVERFLOW_AGENT"`
	WorkspaceCodexOverflowAgent  string `env:"KAGENT_WORKSPACE_CODEX_OVERFLOW_AGENT"`
}

// Tasks limits delegated work.
type Tasks struct {
	DefaultProviderProfileID string `env:"TASK_DEFAULT_PROVIDER_PROFILE_ID" envDefault:"claude"`
	MaxChildrenPerParent     int    `env:"TASK_MAX_CHILDREN_PER_PARENT" envDefault:"3"`
	MaxChildrenGlobal        int    `env:"TASK_MAX_CHILDREN_GLOBAL" envDefault:"6"`
	// MaxWorkersPerProject caps a project's worker tasks (its child tasks) holding a slot, for a
	// project that sets no limit of its own (projects.max_workers).
	MaxWorkersPerProject int `env:"TASK_MAX_WORKERS_PER_PROJECT" envDefault:"6"`

	// MaxChildrenOverflow is the overflow tier above MaxChildrenGlobal: with it above 0, admission
	// takes children up to MaxChildrenGlobal+MaxChildrenOverflow, and a child given an overflow slot
	// waits for runtime capacity (a spare worker waking) for up to OverflowWait before it fails.
	// 0, the default, is no overflow tier.
	MaxChildrenOverflow int     `env:"TASK_MAX_CHILDREN_OVERFLOW" envDefault:"0"`
	OverflowWait        Seconds `env:"TASK_OVERFLOW_WAIT_SECONDS" envDefault:"900"`

	ReconcileInterval Seconds `env:"TASK_RECONCILE_SECONDS" envDefault:"5"`
	// ProvisionWindow is how long a new attempt may wait for its workspace before it is aborted.
	ProvisionWindow Seconds `env:"TASK_PROVISION_WINDOW_SECONDS" envDefault:"900"`
	// MaxHandoffs is how many retries and reassigns main and supervisors may make of one task. The
	// owner decides after that.
	MaxHandoffs int `env:"TASK_MAX_HANDOFFS" envDefault:"3"`
	// ProjectionEvery is how often the GitHub observation of code tasks runs.
	ProjectionEvery Seconds `env:"TASK_PROJECTION_SECONDS" envDefault:"60"`
	// StallTimeout is how long a running task's attempt may show no activity before the liveness
	// sweep blocks it (reason stalled). MaxRuntime is the same sweep's overall safety net, from the
	// attempt's start (reason runtime_limit).
	StallTimeout Seconds `env:"TASK_STALL_TIMEOUT_SECONDS" envDefault:"2700"`
	MaxRuntime   Seconds `env:"TASK_MAX_RUNTIME_SECONDS" envDefault:"21600"`
	// ForceSuspendAfter and ForceDeleteAfter are how long a requested stop may stay unconfirmed before
	// the resolver suspends the runtime, and then deletes the native session. Both count from the stop
	// request. A turn still working cannot be suspended, so it is deleted at ForceDeleteAfter.
	ForceSuspendAfter Seconds `env:"TASK_FORCE_SUSPEND_AFTER_SECONDS" envDefault:"120"`
	ForceDeleteAfter  Seconds `env:"TASK_FORCE_DELETE_AFTER_SECONDS" envDefault:"300"`
}

// Workspaces tunes workspace lifecycle loops.
type Workspaces struct {
	IdleCheckInterval  Seconds `env:"WORKSPACE_IDLE_CHECK_SECONDS" envDefault:"60"`
	CreateRetryInitial Seconds `env:"WORKSPACE_CREATE_RETRY_INITIAL_SECONDS" envDefault:"15"`
	CreateRetryMax     Seconds `env:"WORKSPACE_CREATE_RETRY_MAX_SECONDS" envDefault:"120"`
	CreateRetryWindow  Seconds `env:"WORKSPACE_CREATE_RETRY_WINDOW_SECONDS" envDefault:"600"`
	// ArchivedDeleteInterval is how often kagent Sessions of archived sessions are deleted.
	ArchivedDeleteInterval Seconds `env:"WORKSPACE_ARCHIVED_DELETE_SECONDS" envDefault:"60"`
	// OrphanSweepInterval is how often unreferenced kagent Sessions Mainloop created are deleted.
	OrphanSweepInterval Seconds `env:"WORKSPACE_ORPHAN_SWEEP_SECONDS" envDefault:"3600"`
	// OrphanMinAge keeps the orphan sweep away from Sessions younger than this, which may belong
	// to a create whose reply is still being recorded.
	OrphanMinAge Seconds `env:"WORKSPACE_ORPHAN_MIN_AGE_SECONDS" envDefault:"600"`
	// OperationWait is how long a request waits for the operation it queued before it answers with
	// what is known; the operation carries on in the background.
	OperationWait Seconds `env:"WORKSPACE_OPERATION_WAIT_SECONDS" envDefault:"30"`
}

// MainThread tunes the native main thread.
type MainThread struct {
	// Provider is the first-use main provider, claude or codex. An existing main keeps its own.
	Provider         string `env:"MAIN_PROVIDER" envDefault:"claude"`
	NativeChildKinds string `env:"NATIVE_CHILD_KINDS" envDefault:"claude,codex"`
}

// PushGateDiskBytes leaves room for filesystem overhead and the free-space reserve on the
// gate's 3 GiB spool volume. It bounds both each quarantine and process-wide body reservations.
const PushGateDiskBytes int64 = 2 << 30

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

// Preview configures workspace previews: the wildcard base URL and the Substrate router the
// proxy and the live relay tunnel through.
type Preview struct {
	// RouterAddress is the router's CONNECT-over-TLS listener (Service port 8444). The router has
	// no plain-HTTP listener.
	RouterAddress string `env:"SUBSTRATE_ROUTER_ADDRESS" envDefault:"https://atenet-router.ate-system.svc.cluster.local:8444"`
	// RouterServerName is the name the router's serving certificate is verified against.
	RouterServerName string `env:"SUBSTRATE_ROUTER_SERVER_NAME" envDefault:"atenet-router.ate-system.svc"`
	// RouterClientCredential is the pod certificate the backend presents to the router: one PEM
	// file holding the private key and the certificate chain (signer
	// podidentity.podcert.ate.dev/identity). The kubelet rotates it, so it is read on every connect.
	RouterClientCredential string `env:"SUBSTRATE_ROUTER_CLIENT_CREDENTIAL" envDefault:"/run/substrate-router/identity/credential-bundle.pem"`
	// RouterCA is the trust bundle for the router's serving certificate (signer
	// servicedns.podcert.ate.dev/identity), also read on every connect.
	RouterCA       string  `env:"SUBSTRATE_ROUTER_CA" envDefault:"/run/substrate-router/ca/trust-bundle.pem"`
	BaseURL        string  `env:"SUBSTRATE_PREVIEW_BASE_URL" envDefault:"http://localhost:8001"`
	ConnectTimeout Seconds `env:"SUBSTRATE_PREVIEW_CONNECT_TIMEOUT_SECONDS" envDefault:"5"`
	// ScreenshotBrowserURL is the DevTools endpoint (http://host:9222) of the headless browser that
	// takes preview screenshots for workspace agents. Empty turns preview_screenshot off.
	ScreenshotBrowserURL string `env:"MAINLOOP_SCREENSHOT_BROWSER_URL"`
	// ScreenshotsPerMinute bounds the screenshots one workspace takes a minute.
	ScreenshotsPerMinute int `env:"MAINLOOP_SCREENSHOTS_PER_MINUTE" envDefault:"10"`
}

// Transcripts bounds stored session transcripts (docs/specs/transcripts.md).
type Transcripts struct {
	// RetentionDays is how long a copied turn's entries are kept.
	RetentionDays int `env:"TRANSCRIPT_RETENTION_DAYS" envDefault:"90"`
	// SessionBudgetMiB bounds the entry text stored for one kagent Session; past it, entries are
	// stored as stubs without text.
	SessionBudgetMiB int `env:"TRANSCRIPT_SESSION_BUDGET_MIB" envDefault:"20"`
}

// Health sets orchestration-health retention.
type Health struct {
	EventRetentionDays   int `env:"HEALTH_EVENT_RETENTION_DAYS" envDefault:"30"`
	SummaryRetentionDays int `env:"HEALTH_SUMMARY_RETENTION_DAYS" envDefault:"365"`
}

// Events tunes the event outbox and the SSE stream.
type Events struct {
	// RetentionHours is how long stored events stay replayable through Last-Event-ID.
	RetentionHours int `env:"MAINLOOP_EVENTS_RETENTION_HOURS" envDefault:"24"`
	// SubscriberBuffer is how many events one SSE client may lag behind before it is dropped and
	// must resume from its last event id.
	SubscriberBuffer int `env:"MAINLOOP_EVENTS_SUBSCRIBER_BUFFER" envDefault:"64"`
	// PollInterval is the outbox poll that backs up LISTEN/NOTIFY.
	PollInterval Seconds `env:"MAINLOOP_EVENTS_POLL_SECONDS" envDefault:"5"`
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
	// MCPHosts are the Host names the MCP listener answers to (comma-separated, any port). Empty
	// means the in-cluster Service name of this instance.
	MCPHosts string `env:"MAINLOOP_MCP_HOSTS"`
	// KubernetesServiceHost is set by Kubernetes in every pod; it tells the process it can reach
	// the cluster API.
	KubernetesServiceHost string `env:"KUBERNETES_SERVICE_HOST"`
	// MetricsAddr is where serve-api serves Prometheus metrics (GET /metrics), unauthenticated and
	// apart from the API. The metrics carry counts and limits only, no task or owner data. "off"
	// turns the listener off (an empty value takes the default); see [Config.MetricsListenAddr].
	MetricsAddr string `env:"MAINLOOP_METRICS_ADDR" envDefault:":9100"`
}

// MetricsListenAddr is the metrics listener's address, or "" when MAINLOOP_METRICS_ADDR is off.
func (c *Config) MetricsListenAddr() string {
	if c.Server.MetricsAddr == "off" {
		return ""
	}
	return c.Server.MetricsAddr
}

// Ntfy configures push notifications to the owner's phone (docs/specs/push-notifications.md).
// Push is off unless URL and Topic are both set.
type Ntfy struct {
	// URL is the ntfy server's base URL, such as http://ntfy.ntfy.svc.
	URL   string `env:"MAINLOOP_NTFY_URL"`
	Topic string `env:"MAINLOOP_NTFY_TOPIC"`
	// Token is an ntfy access token with read and write access to Topic (reads let a retry check
	// whether an earlier attempt already arrived). Optional for an open server.
	Token Secret `env:"MAINLOOP_NTFY_TOKEN"`
}

// Enabled reports whether push notifications are configured.
func (n Ntfy) Enabled() bool { return n.URL != "" && n.Topic != "" }

// APNs configures Apple push to the owner's registered phones (docs/specs/push-notifications.md).
// The credentials are files in CredentialsDir, from a Secret mounted optionally: until it exists,
// APNs is off. serve-executor reads them at startup.
type APNs struct {
	// CredentialsDir holds key.p8 (the token-auth key, PEM), key-id and team-id.
	CredentialsDir string `env:"MAINLOOP_APNS_CREDENTIALS_DIR"`
	// Topic is the app's bundle ID.
	Topic string `env:"MAINLOOP_APNS_TOPIC"`
}

// Auth is how the owner API verifies who is calling (docs/specs/authentication.md). Only serve-api
// reads it; [Auth.Mode] must be set there outside development.
type Auth struct {
	// Mode is off, observe (verify and log, refuse nothing) or enforce. Empty is off in development
	// and a startup error for serve-api elsewhere.
	Mode string `env:"MAINLOOP_AUTH_MODE"`
	// OIDCIssuer is the Pocket ID issuer whose tokens identify the owner, such as
	// https://id.example.com. Its keys come from the issuer's discovery document.
	OIDCIssuer string `env:"MAINLOOP_AUTH_OIDC_ISSUER"`
	// OIDCAudience is the audience (the OIDC client id) an owner token must carry.
	OIDCAudience string `env:"MAINLOOP_AUTH_OIDC_AUDIENCE"`
	// OIDCCookie is the gateway's login cookie holding the ID token.
	OIDCCookie string `env:"MAINLOOP_AUTH_OIDC_COOKIE" envDefault:"mainloop-id"`
	// DeviceClientID is the native app's public OIDC client (PKCE). A token with only this audience
	// may register a device and call nothing else (docs/specs/devices.md).
	DeviceClientID string `env:"MAINLOOP_AUTH_DEVICE_CLIENT_ID"`
	// OwnerSubject is the owner's Pocket ID subject (sub). Only that subject is the owner.
	OwnerSubject string `env:"MAINLOOP_AUTH_OWNER_SUBJECT"`
	// OwnerGroup, when set, is a group the owner token's groups claim must also contain.
	OwnerGroup string `env:"MAINLOOP_AUTH_OWNER_GROUP"`
	// ServiceAccountAudience is the audience a ServiceAccount token must be issued for.
	ServiceAccountAudience string `env:"MAINLOOP_AUTH_SA_AUDIENCE" envDefault:"mainloop-api"`
	// ServiceAccountList allows ServiceAccounts, comma-separated namespace/name=scope with scope
	// owner, read or canary. Parsed into ServiceAccounts.
	ServiceAccountList string `env:"MAINLOOP_AUTH_SERVICE_ACCOUNTS"`
	// ServiceAccounts maps namespace/name to its scope.
	ServiceAccounts map[string]string
}

// AuthScopes are the scopes a ServiceAccount can be given.
var AuthScopes = map[string]bool{"owner": true, "read": true, "canary": true}

// validate checks the syntax of the auth settings; whether they suffice for the mode is serve-api's
// check, since other processes share the config and never authenticate owners.
func (a *Auth) validate(dev bool) []error {
	var errs []error
	a.Mode = strings.TrimSpace(a.Mode)
	switch a.Mode {
	case "", "off", "observe", "enforce":
	default:
		errs = append(errs, errors.New("MAINLOOP_AUTH_MODE must be off, observe or enforce"))
	}
	a.OIDCIssuer = strings.TrimRight(strings.TrimSpace(a.OIDCIssuer), "/")
	if a.OIDCIssuer != "" {
		u, err := url.Parse(a.OIDCIssuer)
		if err != nil || u.Host == "" || (u.Scheme != "https" && (!dev || u.Scheme != "http")) {
			errs = append(errs, errors.New("MAINLOOP_AUTH_OIDC_ISSUER must be an https URL"))
		}
	}
	a.OIDCAudience = strings.TrimSpace(a.OIDCAudience)
	a.DeviceClientID = strings.TrimSpace(a.DeviceClientID)
	if a.DeviceClientID != "" && a.OIDCIssuer == "" {
		errs = append(errs, errors.New("MAINLOOP_AUTH_DEVICE_CLIENT_ID needs MAINLOOP_AUTH_OIDC_ISSUER"))
	}
	if a.DeviceClientID != "" && a.DeviceClientID == a.OIDCAudience {
		errs = append(errs, errors.New("MAINLOOP_AUTH_DEVICE_CLIENT_ID must differ from MAINLOOP_AUTH_OIDC_AUDIENCE"))
	}
	a.OIDCCookie = strings.TrimSpace(a.OIDCCookie)
	a.OwnerSubject = strings.TrimSpace(a.OwnerSubject)
	a.OwnerGroup = strings.TrimSpace(a.OwnerGroup)
	a.ServiceAccountAudience = strings.TrimSpace(a.ServiceAccountAudience)
	if (a.OIDCIssuer != "") != (a.OIDCAudience != "") {
		errs = append(errs, errors.New("MAINLOOP_AUTH_OIDC_ISSUER and MAINLOOP_AUTH_OIDC_AUDIENCE must be set together"))
	}
	if a.OIDCIssuer != "" && a.OwnerSubject == "" {
		errs = append(errs, errors.New("MAINLOOP_AUTH_OWNER_SUBJECT is required with MAINLOOP_AUTH_OIDC_ISSUER"))
	}
	a.ServiceAccounts = map[string]string{}
	for _, entry := range strings.Split(a.ServiceAccountList, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, scope, ok := strings.Cut(entry, "=")
		ns, sa, okName := strings.Cut(strings.TrimSpace(name), "/")
		scope = strings.TrimSpace(scope)
		if !ok || !okName || !dnsLabel.MatchString(ns) || !serviceAccountName.MatchString(sa) || !AuthScopes[scope] {
			errs = append(errs, fmt.Errorf("MAINLOOP_AUTH_SERVICE_ACCOUNTS: %q is not namespace/name=owner|read|canary", entry))
			continue
		}
		a.ServiceAccounts[ns+"/"+sa] = scope
	}
	if len(a.ServiceAccounts) > 0 && a.ServiceAccountAudience == "" {
		errs = append(errs, errors.New("MAINLOOP_AUTH_SA_AUDIENCE must not be blank"))
	}
	return errs
}

var serviceAccountName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

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

var ntfyTopic = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// bundleID is an Apple bundle ID: reverse-DNS labels of letters, digits and '-'.
var bundleID = regexp.MustCompile(`^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)

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
	errs = append(errs, c.Auth.validate(c.IsDev())...)
	c.Ntfy.URL = strings.TrimRight(strings.TrimSpace(c.Ntfy.URL), "/")
	c.Ntfy.Topic = strings.TrimSpace(c.Ntfy.Topic)
	if (c.Ntfy.URL != "") != (c.Ntfy.Topic != "") {
		fail("MAINLOOP_NTFY_URL and MAINLOOP_NTFY_TOPIC must be set together")
	} else if c.Ntfy.URL != "" {
		if u, err := url.Parse(c.Ntfy.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fail("MAINLOOP_NTFY_URL must be an http(s) URL")
		}
		if !ntfyTopic.MatchString(c.Ntfy.Topic) {
			fail("MAINLOOP_NTFY_TOPIC must be 1-64 letters, digits, '-' or '_'")
		}
	}
	c.APNs.CredentialsDir = strings.TrimSpace(c.APNs.CredentialsDir)
	c.APNs.Topic = strings.TrimSpace(c.APNs.Topic)
	if c.APNs.CredentialsDir != "" && !bundleID.MatchString(c.APNs.Topic) {
		fail("MAINLOOP_APNS_TOPIC must be the app's bundle ID when MAINLOOP_APNS_CREDENTIALS_DIR is set")
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
	for _, h := range splitHosts(c.Server.MCPHosts) {
		if _, ok := Hostname(h); !ok {
			fail("MAINLOOP_MCP_HOSTS entry %q is not a host name", h)
		}
	}
	// Agents are handed MCPOrigin() in a cluster; the MCP listener refuses every other Host, so an
	// allowlist without the Service's host would make every agent's MCP call fail.
	if c.InCluster() {
		if u, err := url.Parse(c.MCPOrigin()); err != nil || !c.AllowedMCPHosts()[u.Hostname()] {
			fail("MAINLOOP_MCP_HOSTS must include %s, the host agents are given as the MCP origin", c.MCPOrigin())
		}
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
	if c.Tasks.MaxWorkersPerProject < 1 || c.Tasks.MaxWorkersPerProject > 100 {
		fail("TASK_MAX_WORKERS_PER_PROJECT must be between 1 and 100")
	}
	if c.Tasks.MaxChildrenOverflow < 0 {
		fail("TASK_MAX_CHILDREN_OVERFLOW must be at least 0")
	}
	if c.Tasks.OverflowWait.Duration() < time.Minute {
		fail("TASK_OVERFLOW_WAIT_SECONDS must be at least 60")
	}
	if c.Tasks.MaxHandoffs < 1 {
		fail("TASK_MAX_HANDOFFS must be at least 1")
	}
	if !validProviderID(c.Tasks.DefaultProviderProfileID) {
		fail("TASK_DEFAULT_PROVIDER_PROFILE_ID must match ^[a-z][a-z0-9_-]*$")
	}
	if c.Tasks.StallTimeout <= 0 || c.Tasks.MaxRuntime <= 0 {
		fail("TASK_STALL_TIMEOUT_SECONDS and TASK_MAX_RUNTIME_SECONDS must be positive")
	}
	if c.Tasks.MaxRuntime < c.Tasks.StallTimeout {
		fail("TASK_MAX_RUNTIME_SECONDS must be at least TASK_STALL_TIMEOUT_SECONDS")
	}
	if c.Tasks.ForceSuspendAfter <= 0 || c.Tasks.ForceDeleteAfter <= 0 {
		fail("TASK_FORCE_SUSPEND_AFTER_SECONDS and TASK_FORCE_DELETE_AFTER_SECONDS must be positive")
	}
	if c.Tasks.ForceDeleteAfter < c.Tasks.ForceSuspendAfter {
		fail("TASK_FORCE_DELETE_AFTER_SECONDS must be at least TASK_FORCE_SUSPEND_AFTER_SECONDS")
	}
	if c.Main.Provider != NativeClaude && c.Main.Provider != NativeCodex {
		fail("MAIN_PROVIDER must be claude or codex")
	}
	if c.Sessions.CarryOverMessages < 0 || c.Sessions.CarryOverMessages > 100 {
		fail("MAIN_CARRY_OVER_MESSAGES must be between 0 and 100")
	}
	if c.Git.PushGateMaxCommandBytes < 1 || c.Git.PushGateMaxBodyBytes < 1 {
		fail("PUSH_GATE_MAX_COMMAND_BYTES and PUSH_GATE_MAX_BODY_BYTES must be at least 1")
	}
	if c.Git.PushGateMaxBodyBytes > PushGateDiskBytes {
		fail("PUSH_GATE_MAX_BODY_BYTES must not exceed the 2 GiB scratch disk budget")
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
	if c.Transcripts.RetentionDays < 1 || c.Transcripts.RetentionDays > 3650 {
		fail("TRANSCRIPT_RETENTION_DAYS must be between 1 and 3650")
	}
	if c.Transcripts.SessionBudgetMiB < 1 || c.Transcripts.SessionBudgetMiB > 1024 {
		fail("TRANSCRIPT_SESSION_BUDGET_MIB must be between 1 and 1024")
	}
	if c.Events.RetentionHours < 1 || c.Events.RetentionHours > 720 {
		fail("MAINLOOP_EVENTS_RETENTION_HOURS must be between 1 and 720")
	}
	if c.Events.SubscriberBuffer < 8 || c.Events.SubscriberBuffer > 4096 {
		fail("MAINLOOP_EVENTS_SUBSCRIBER_BUFFER must be between 8 and 4096")
	}
	if c.Sessions.DeliveryConcurrency < 1 || c.Sessions.DeliveryConcurrency > 256 {
		fail("SESSION_DELIVERY_CONCURRENCY must be between 1 and 256")
	}
	if i := c.EnvironmentFollowInterval.Duration(); i < time.Minute || i > 7*24*time.Hour {
		fail("ENVIRONMENT_FOLLOW_MIN_INTERVAL_SECONDS must be between 60 and 604800")
	}
	if c.WorkspaceDevelopmentPlatform != "linux/arm64" && c.WorkspaceDevelopmentPlatform != "linux/amd64" {
		fail("WORKSPACE_DEVELOPMENT_PLATFORM must be linux/arm64 or linux/amd64")
	}
	w := c.Workspaces
	if w.CreateRetryMax.Duration() < w.CreateRetryInitial.Duration() {
		fail("WORKSPACE_CREATE_RETRY_MAX_SECONDS must be at least WORKSPACE_CREATE_RETRY_INITIAL_SECONDS")
	}
	if w.CreateRetryWindow.Duration() < w.CreateRetryInitial.Duration() {
		fail("WORKSPACE_CREATE_RETRY_WINDOW_SECONDS must be at least WORKSPACE_CREATE_RETRY_INITIAL_SECONDS")
	}
	errs = append(errs, c.Kagent.validate()...)
	errs = append(errs, c.Preview.validate(c.Kagent.ActorAtespace)...)
	return errors.Join(errs...)
}

// IsDev reports whether loopback Hosts and localhost Origins are accepted.
func (c *Config) IsDev() bool { return c.Identity.DevMode || c.Identity.IsTestEnv }

// PrivilegedActionsOn reports whether privileged actions run: enabled, and owner authentication
// enforced.
func (c *Config) PrivilegedActionsOn() bool {
	return bool(c.PrivilegedActionsEnabled) && c.Auth.Mode == "enforce"
}

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

// InCluster reports whether the process runs in a Kubernetes pod.
func (c *Config) InCluster() bool { return c.Server.KubernetesServiceHost != "" }

// AllowedMCPHosts returns the lower-case host names the MCP listener serves besides loopback:
// MAINLOOP_MCP_HOSTS, or the instance's in-cluster Service name when it is unset.
func (c *Config) AllowedMCPHosts() map[string]bool {
	if names := splitHosts(c.Server.MCPHosts); len(names) > 0 {
		return hostSet(names)
	}
	return hostSet([]string{"mainloop-mcp." + c.Namespace + ".svc.cluster.local"})
}

// MCPOrigin is the in-cluster URL agents reach the MCP server at: the instance's Service name over
// plain HTTP, which is also the origin the credential Secrets are bound to.
func (c *Config) MCPOrigin() string {
	return c.ServiceOrigin("mainloop-mcp")
}

// splitHosts splits a comma-separated host list and drops blank entries.
func splitHosts(list string) []string {
	var out []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
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

// GitReadHosts returns the lower-case host names the git read listener serves besides loopback:
// the instance's in-cluster Service name, which is the origin agents' git is pointed at.
func (c *Config) GitReadHosts() map[string]bool {
	return hostSet([]string{"mainloop-git-read." + c.Namespace + ".svc.cluster.local"})
}

// GitPushHosts is [Config.GitReadHosts] for the push listener.
func (c *Config) GitPushHosts() map[string]bool {
	return hostSet([]string{"mainloop-git-push." + c.Namespace + ".svc.cluster.local"})
}

// APITokenFile is MAINLOOP_API_TOKEN_FILE: the file holding the bearer a machine caller (`mainloop
// canary`, `mainloop smoke`) sends to the owner API. The chart sets it to the pod's projected
// ServiceAccount token; empty outside such a pod.
func APITokenFile() string {
	var v struct {
		File string `env:"MAINLOOP_API_TOKEN_FILE"`
	}
	_ = env.ParseWithOptions(&v, env.Options{Environment: environ()})
	return strings.TrimSpace(v.File)
}

// LoadNtfy reads only the ntfy settings, for commands such as `mainloop canary` that need no
// database or agent key.
func LoadNtfy() (Ntfy, error) {
	var n Ntfy
	if err := env.ParseWithOptions(&n, env.Options{Environment: environ()}); err != nil {
		return n, fmt.Errorf("config: %w", err)
	}
	n.URL = strings.TrimRight(strings.TrimSpace(n.URL), "/")
	n.Topic = strings.TrimSpace(n.Topic)
	if (n.URL != "") != (n.Topic != "") {
		return n, errors.New("config: MAINLOOP_NTFY_URL and MAINLOOP_NTFY_TOPIC must be set together")
	}
	if n.Topic != "" && !ntfyTopic.MatchString(n.Topic) {
		return n, errors.New("config: MAINLOOP_NTFY_TOPIC must be 1-64 letters, digits, '-' or '_'")
	}
	return n, nil
}
