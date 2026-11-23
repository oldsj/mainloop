package tasks

import "fmt"

// Error is a refusal the caller is shown: an HTTP status and the stable machine code that goes in
// the task error body. Detail carries extra TaskErrorDetail fields, for example the allowed
// values of a model_unavailable or effort_unavailable refusal. Anything that is not an *Error is
// an internal failure.
type Error struct {
	Status int
	Reason string
	Detail map[string]any
}

func (e *Error) Error() string { return fmt.Sprintf("tasks: %d %s", e.Status, e.Reason) }

func refusal(status int, reason string) *Error { return &Error{Status: status, Reason: reason} }

// refusalDetail is [refusal] with extra TaskErrorDetail fields.
func refusalDetail(status int, reason string, detail map[string]any) *Error {
	return &Error{Status: status, Reason: reason, Detail: detail}
}

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
	ReasonReportScreenshots    = "report_screenshots"
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
	ReasonProjectCapacity      = "project_capacity"
	ReasonHoldNotLive          = "hold_task_not_live"
	ReasonHoldNeedsOwner       = "hold_task_needs_owner"
	ReasonGlobalCapacity       = "global_capacity"
	ReasonModelUnavailable     = "model_unavailable"
	ReasonEffortUnavailable    = "effort_unavailable"
	ReasonTaskNotRunning       = "task_not_running"
	// ReasonStandingSupervisor refuses an action a project's standing supervisor does not take:
	// it is never cancelled, and is restarted rather than retried or reassigned.
	ReasonStandingSupervisor = "standing_supervisor"
	// ReasonStandingOutcome refuses a failed report from a standing supervisor, which never fails:
	// it reports blocked instead.
	ReasonStandingOutcome = "standing_outcome"
	// ReasonProjectSupervisor refuses main's delegate in a project: project work goes to the
	// project's standing supervisor (supervisor_send), which delegates it.
	ReasonProjectSupervisor = "project_supervisor"
	// ReasonSupervisorStarting refuses a supervisor_send whose supervisor has not been given its
	// first brief yet; the same request_id is sent again a moment later.
	ReasonSupervisorStarting = "supervisor_starting"
	// Follow-up refusals; see [Service.FollowUp].
	ReasonSessionGone       = "session_gone"
	ReasonSessionArchived   = "session_archived"
	ReasonTaskSettling      = "task_settling"
	ReasonOperationPending  = "operation_pending"
	ReasonFollowUpCode      = "follow_up_code_task_completed"
	ReasonRuntimeLimitOwner = "runtime_limit_needs_owner"
	// ReasonHandoffLimit refuses a retry or reassign from main or a supervisor once the task has had
	// Limits.MaxHandoffs of them. The owner decides what happens next and is never limited.
	ReasonHandoffLimit = "handoff_limit"
)
