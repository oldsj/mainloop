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
