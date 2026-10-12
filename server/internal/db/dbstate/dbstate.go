// Package dbstate holds the Go types of the text columns that the schema constrains with a CHECK.
// sqlc maps those columns to these types. The set of values here must match the CHECK in
// migrations; a test compares them. Allowed transitions belong to the domain package that owns
// the state machine.
package dbstate

// SessionStatus is sessions.status.
type SessionStatus string

// Session statuses.
const (
	SessionPending       SessionStatus = "pending"
	SessionActive        SessionStatus = "active"
	SessionIdle          SessionStatus = "idle"
	SessionWaitingOnUser SessionStatus = "waiting_on_user"
	SessionSuperseded    SessionStatus = "superseded"
	SessionCompleted     SessionStatus = "completed"
	SessionFailed        SessionStatus = "failed"
	SessionCancelled     SessionStatus = "cancelled"
)

// BindingRole is agent_bindings.role.
type BindingRole string

// Binding roles.
const (
	RoleMain       BindingRole = "main"
	RoleChild      BindingRole = "child"
	RoleAgent      BindingRole = "agent"
	RoleSupervisor BindingRole = "supervisor"
)

// MCPGrantKind is agent_bindings.mcp_grant_kind.
type MCPGrantKind string

// MCP grant kinds.
const (
	GrantNone         MCPGrantKind = "none"
	GrantCoordination MCPGrantKind = "coordination"
	GrantWorkspace    MCPGrantKind = "workspace"
)

// CredentialKind is credentials.kind.
type CredentialKind string

// Credential kinds.
const (
	CredentialMCP     CredentialKind = "mcp"
	CredentialGitRead CredentialKind = "git_read"
	CredentialGitPush CredentialKind = "git_push"
)

// CredentialState is credentials.state: planned, then published, then revoked (Secret delete
// owed), then deleted.
type CredentialState string

// Credential states.
const (
	CredentialPlanned   CredentialState = "planned"
	CredentialPublished CredentialState = "published"
	CredentialRevoked   CredentialState = "revoked"
	CredentialDeleted   CredentialState = "deleted"
)

// MergePolicy is projects.merge_policy.
type MergePolicy string

// Merge policies.
const (
	MergeAuto     MergePolicy = "auto"
	MergeApproval MergePolicy = "approval"
)

// TopicRecordKind is topic_records.kind.
type TopicRecordKind string

// Topic record kinds.
const (
	RecordNote     TopicRecordKind = "note"
	RecordDecision TopicRecordKind = "decision"
	RecordPending  TopicRecordKind = "pending"
	RecordReport   TopicRecordKind = "report"
)

// TopicRecordStatus is topic_records.status.
type TopicRecordStatus string

// Topic record statuses.
const (
	RecordOpen TopicRecordStatus = "open"
	RecordDone TopicRecordStatus = "done"
)

// ValidationStatus is environment_versions.validation_status.
type ValidationStatus string

// Environment version validation statuses.
const (
	ValidationPendingBuild    ValidationStatus = "pending_build"
	ValidationStaticValidated ValidationStatus = "static_validated"
)

// ProvenanceKind is environment_versions.provenance_kind.
type ProvenanceKind string

// Environment version provenance kinds.
const (
	ProvenanceUserPushed    ProvenanceKind = "user_pushed"
	ProvenanceMainloopBuilt ProvenanceKind = "mainloop_built"
	ProvenanceOperator      ProvenanceKind = "operator"
)

// BaseProvenance is environment_versions.base_provenance.
type BaseProvenance string

// Base image provenance values.
const (
	BaseSupplied BaseProvenance = "supplied"
	BaseVerified BaseProvenance = "verified"
	BaseUnknown  BaseProvenance = "unknown"
)

// EnvironmentSourceKind is dev_environments.source_kind.
type EnvironmentSourceKind string

// Environment source kinds.
const (
	SourcePrebuiltImage   EnvironmentSourceKind = "prebuilt_image"
	SourceDefinitionRepo  EnvironmentSourceKind = "definition_repo"
	SourceOperatorDefault EnvironmentSourceKind = "operator_default"
)

// GrantPermission is environment_grants.permission.
type GrantPermission string

// Environment grant permissions.
const (
	PermissionUse    GrantPermission = "use"
	PermissionDerive GrantPermission = "derive"
)

// WorkspaceOpKind is workspace_ops.kind.
type WorkspaceOpKind string

// Workspace operation kinds.
const (
	OpCreate  WorkspaceOpKind = "create"
	OpSuspend WorkspaceOpKind = "suspend"
	OpResume  WorkspaceOpKind = "resume"
	OpDelete  WorkspaceOpKind = "delete"
)

// WorkspaceOpState is workspace_ops.state. Pending and retrying are claimable; the rest are
// final. A retrying operation has failed transiently at least once.
type WorkspaceOpState string

// Workspace operation states.
const (
	OpPending   WorkspaceOpState = "pending"
	OpRetrying  WorkspaceOpState = "retrying"
	OpSucceeded WorkspaceOpState = "succeeded"
	// OpRejected means kagent refused the request for good (or the operation can never apply).
	OpRejected WorkspaceOpState = "rejected"
	// OpGaveUp means transient failures outlasted the retry window.
	OpGaveUp    WorkspaceOpState = "gave_up"
	OpCancelled WorkspaceOpState = "cancelled"
)

// Workspace preparation states (workspace_preparations.state).
const (
	PreparationPlanned   = "planned"
	PreparationRequested = "requested"
	PreparationConfirmed = "confirmed"
	PreparationFailed    = "failed"
)
