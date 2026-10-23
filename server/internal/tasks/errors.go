package tasks

import "fmt"

// Error is a refusal the caller is shown: an HTTP status and the stable machine code that goes in
// the task error body. Anything that is not an *Error is an internal failure.
type Error struct {
	Status int
	Reason string
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("tasks: %d %s", e.Status, e.Reason) }

func refusal(status int, reason string) *Error { return &Error{Status: status, Reason: reason} }

// Reasons. The set is the contract of the task routes and the MCP tools.
const (
	ReasonTaskNotFound         = "task_not_found"
	ReasonOperationNotFound    = "operation_not_found"
	ReasonProjectNotFound      = "project_not_found"
	ReasonTopicNotFound        = "topic_not_found"
	ReasonManagementDenied     = "task_management_denied"
	ReasonInheritedProject     = "inherited_project_required"
	ReasonInheritedProvider    = "inherited_provider_constraint"
	ReasonInactivePrincipal    = "inactive_principal"
	ReasonReportScope          = "report_attempt_scope"
	ReasonCompletedOutcome     = "completed_report_outcome"
	ReasonStaleAttempt         = "stale_task_attempt"
	ReasonStaleVersion         = "stale_task_version"
	ReasonPayloadConflict      = "request_payload_conflict"
	ReasonReportConflict       = "report_request_conflict"
	ReasonTaskTerminal         = "task_terminal"
	ReasonCompletedRecorded    = "completed_report_already_recorded"
	ReasonLiveChildren         = "live_children"
	ReasonParentNotActive      = "parent_not_active"
	ReasonCodeNeedsProject     = "code_requires_project"
	ReasonCodeNeedsCheckout    = "code_requires_checkout"
	ReasonEnvironmentRequired  = "project_environment_required"
	ReasonCoordinationCheckout = "coordination_forbids_checkout"
	ReasonProviderUnknown      = "provider_unknown"
	ReasonProviderRole         = "provider_role_unavailable"
	ReasonProviderDisabled     = "provider_disabled"
	ReasonInvalidRequest       = "invalid_request"
	ReasonOwnerActionOutcome   = "owner_action_requires_blocked"
	ReasonInvalidCheckout      = "invalid_checkout"
	ReasonBriefTooLong         = "brief_too_long"
	ReasonChildCapacity        = "child_capacity"
	ReasonGlobalCapacity       = "global_capacity"
)
