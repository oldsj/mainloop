package agentauth

import (
	"slices"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// Tool names. The set is the contract in api/mcp-tools.json.
const (
	ToolWhoami                    = "whoami"
	ToolTopics                    = "topics"
	ToolTopicOpen                 = "topic_open"
	ToolNote                      = "note"
	ToolDecide                    = "decide"
	ToolPendingAdd                = "pending_add"
	ToolPendingDone               = "pending_done"
	ToolOpenPullRequest           = "open_pull_request"
	ToolGetIssue                  = "get_issue"
	ToolGetPullRequest            = "get_pull_request"
	ToolCreateIssue               = "create_issue"
	ToolCommentIssue              = "comment_issue"
	ToolCommentPullRequest        = "comment_pull_request"
	ToolListDependabotAlerts      = "list_dependabot_alerts"
	ToolCheckStatus               = "check_status"
	ToolWatchChecks               = "watch_checks"
	ToolGetVerification           = "get_verification"
	ToolProjectContext            = "project_context"
	ToolRemember                  = "remember"
	ToolForget                    = "forget"
	ToolProjectPropose            = "project_propose"
	ToolTaskGet                   = "task_get"
	ToolTaskList                  = "task_list"
	ToolTaskHistory               = "task_history"
	ToolTaskTranscript            = "task_transcript"
	ToolTranscriptSearch          = "transcript_search"
	ToolDelegate                  = "delegate"
	ToolListModels                = "list_models"
	ToolTaskCancel                = "task_cancel"
	ToolTaskRetry                 = "task_retry"
	ToolTaskReassign              = "task_reassign"
	ToolTaskHold                  = "task_hold"
	ToolSteer                     = "steer"
	ToolFollowUp                  = "follow_up"
	ToolSupervisorSend            = "supervisor_send"
	ToolProjectSettings           = "project_settings"
	ToolReport                    = "report"
	ToolLoopArm                   = "loop_arm"
	ToolLoopList                  = "loop_list"
	ToolLoopStop                  = "loop_stop"
	ToolPreparePullRequestMerge   = "prepare_pull_request_merge"
	ToolMergePullRequest          = "merge_pull_request"
	ToolMergePullRequestApproval  = "merge_pull_request_with_approval"
	ToolGetPullRequestMergeStatus = "get_pull_request_merge_status"
	ToolDevLeaseAcquire           = "dev_lease_acquire"
	ToolDevLeaseRelease           = "dev_lease_release"
	ToolDevSync                   = "dev_sync"
	ToolDevStatus                 = "dev_status"
	ToolWebFetch                  = "web_fetch"
)

// AllTools lists every tool, in the order api/mcp-tools.json lists them.
var AllTools = []string{
	ToolWhoami, ToolTopics, ToolTopicOpen, ToolNote, ToolDecide, ToolPendingAdd, ToolPendingDone,
	ToolOpenPullRequest,
	ToolGetIssue, ToolGetPullRequest, ToolCreateIssue, ToolCommentIssue, ToolCommentPullRequest, ToolListDependabotAlerts, ToolCheckStatus, ToolWatchChecks, ToolGetVerification,
	ToolProjectContext, ToolRemember, ToolForget, ToolProjectPropose,
	ToolTaskGet, ToolTaskList, ToolTaskHistory, ToolTaskTranscript, ToolTranscriptSearch, ToolDelegate, ToolListModels, ToolTaskCancel, ToolTaskRetry, ToolTaskReassign,
	ToolTaskHold, ToolSteer, ToolFollowUp, ToolSupervisorSend, ToolProjectSettings, ToolReport, ToolLoopArm, ToolLoopList, ToolLoopStop,
	ToolPreparePullRequestMerge, ToolMergePullRequest, ToolMergePullRequestApproval,
	ToolGetPullRequestMergeStatus,
	ToolDevLeaseAcquire, ToolDevLeaseRelease, ToolDevSync, ToolDevStatus,
	ToolWebFetch,
}

// Surface is one of the two MCP endpoints. The approval-gated merge tool is served alone on its
// own path so a client can be pointed at it without the ordinary tools.
type Surface string

// The two surfaces.
const (
	SurfaceOrdinary Surface = "ordinary" // /mcp
	SurfaceApproval Surface = "approval" // /mcp/merge-approval
)

// Actor is the authenticated agent behind a request.
type Actor struct {
	CredentialID string
	// BindingID is the agent's session id.
	BindingID string
	UserID    string
	ProjectID string
	// AgentKind is the native agent kind ("claude", "codex").
	AgentKind string
	Role      dbstate.BindingRole
	Grant     dbstate.MCPGrantKind
	// Depth is 0 for the main thread and workspace agents, 1 for task supervisors and 2 for task
	// children.
	Depth int
	// MergeStatusOnly marks the actor of a completed task: it may only look at merge status.
	MergeStatusOnly bool
	// WebFetch is set when the owner turned on the open-web web_fetch tool for this agent: its
	// session's flag, or the flag of the task it runs (docs/specs/web-fetch.md).
	WebFetch bool
}

// ValidPair reports whether a binding role may hold an MCP grant of this kind. The same set
// gates credential issue and every request.
func ValidPair(role dbstate.BindingRole, grant dbstate.MCPGrantKind) bool {
	switch role {
	case dbstate.RoleMain, dbstate.RoleSupervisor, dbstate.RoleChild:
		if grant == dbstate.GrantCoordination {
			return true
		}
		// Task attempts own the workspace grant; a main thread never does.
		return role != dbstate.RoleMain && grant == dbstate.GrantWorkspace
	case dbstate.RoleAgent:
		return grant == dbstate.GrantWorkspace
	}
	return false
}

// Policy decides which tools an actor sees. Discovery and invocation both use it, so a tool an
// actor cannot see is a tool it cannot call.
type Policy struct {
	// MergeTools turns the merge tools on (MAINLOOP_MERGE_TOOLS_ENABLED=true).
	MergeTools bool
	// DevLoop turns the devloop tools on (MAINLOOP_DEVLOOP_ENABLED=true). A handler-level check
	// further limits them to tasks of the one allowed project; the flag alone decides visibility.
	DevLoop bool
}

var (
	taskRead = []string{ToolTaskGet, ToolTaskList, ToolTaskHistory, ToolTaskTranscript, ToolTranscriptSearch}
	// list_models goes with delegate and task_reassign: it lists the exact providers, models and
	// effort levels they accept.
	taskManagement = []string{ToolDelegate, ToolListModels, ToolTaskCancel, ToolTaskRetry, ToolTaskReassign, ToolSteer, ToolFollowUp}
	// loopTools are main's and supervisors' wake-ups: what fires is a message to them.
	loopTools      = []string{ToolLoopArm, ToolLoopList, ToolLoopStop}
	workspaceTools = []string{ToolWhoami, ToolOpenPullRequest}
	mergeTools     = []string{
		ToolPreparePullRequestMerge, ToolMergePullRequest, ToolMergePullRequestApproval,
		ToolGetPullRequestMergeStatus,
	}
	devLoopTools = []string{
		ToolDevLeaseAcquire, ToolDevLeaseRelease, ToolDevSync, ToolDevStatus,
	}
	// issueTools are the GitHub issue, pull-request and Dependabot-alert tools of a code task, and
	// checkTools the CI tools. They carry no feature gate of their own: wherever an actor can open a
	// pull request on its bound project, it can also read that project's issues, pull requests,
	// alerts and CI results, and comment on or review (never approve) its pull requests: a
	// reviewer is usually a code task.
	issueTools = []string{ToolGetIssue, ToolGetPullRequest, ToolCommentPullRequest, ToolListDependabotAlerts}
	checkTools = []string{ToolCheckStatus, ToolWatchChecks, ToolGetVerification}
	// issueWriteTools file issues and comments. The coordination grant holds them (the main thread,
	// and coordination supervisors and children), with get_issue and get_pull_request so a comment
	// can follow a read, and comment_pull_request; code tasks report issues they find instead.
	issueWriteTools = []string{ToolGetIssue, ToolGetPullRequest, ToolCreateIssue, ToolCommentIssue, ToolCommentPullRequest}
)

// Tools returns the tool names the actor may use on the surface, in [AllTools] order.
func (p Policy) Tools(a Actor, s Surface) []string {
	set := p.actorTools(a)
	var out []string
	for _, name := range AllTools {
		if !slices.Contains(set, name) {
			continue
		}
		approval := name == ToolMergePullRequestApproval
		if (s == SurfaceApproval) != approval {
			continue
		}
		out = append(out, name)
	}
	return out
}

// Allowed reports whether the actor may use the tool on the surface.
func (p Policy) Allowed(a Actor, s Surface, tool string) bool {
	return slices.Contains(p.Tools(a, s), tool)
}

func (p Policy) actorTools(a Actor) []string {
	if a.MergeStatusOnly {
		if !p.MergeTools {
			return nil
		}
		return []string{ToolGetPullRequestMergeStatus, ToolWhoami, ToolTaskGet, ToolReport}
	}
	if !ValidPair(a.Role, a.Grant) {
		return nil
	}
	var tools []string
	switch {
	case a.Role == dbstate.RoleMain && a.Depth == 0:
		tools = append(tools, ToolWhoami, ToolTopics, ToolTopicOpen, ToolNote, ToolDecide,
			ToolPendingAdd, ToolPendingDone, ToolOpenPullRequest)
		tools = append(tools, taskRead...)
		tools = append(tools, taskManagement...)
		tools = append(tools, loopTools...)
		// Only the main thread holds a task: it is the owner's view that a hold changes. Only main
		// sends to a project's standing supervisor: it is the top of the three layers. Only main
		// changes a project's settings for the owner: a supervisor must not raise its own limit.
		tools = append(tools, ToolTaskHold, ToolSupervisorSend, ToolProjectSettings)
		// Main and supervisors propose new projects; the owner approves each one.
		tools = append(tools, ToolProjectPropose)
	case (a.Role == dbstate.RoleSupervisor && a.Depth == 1) || (a.Role == dbstate.RoleChild && a.Depth == 2):
		tools = append(tools, ToolWhoami, ToolReport)
		tools = append(tools, taskRead...)
		if a.Role == dbstate.RoleSupervisor {
			tools = append(tools, taskManagement...)
			tools = append(tools, loopTools...)
			// A coordinating supervisor (a project's standing one, or an ad-hoc one) may propose a
			// project; a root code task, which also runs as a supervisor, may not.
			if a.Grant == dbstate.GrantCoordination {
				tools = append(tools, ToolProjectPropose)
			}
		}
		if a.Grant == dbstate.GrantWorkspace {
			tools = append(tools, workspaceTools...)
		}
	case a.Role == dbstate.RoleAgent && a.Depth == 0:
		tools = append(tools, workspaceTools...)
	}
	// Every live role reads its project's owner context and records memory on it: main any of its
	// owner's projects, a task or workspace agent its own project only (the handlers check which).
	// Only main's entries are active at once; the others are proposals for the owner.
	if len(tools) > 0 {
		tools = append(tools, ToolProjectContext, ToolRemember, ToolForget)
	}
	if slices.Contains(tools, ToolOpenPullRequest) {
		tools = append(tools, issueTools...)
		tools = append(tools, checkTools...)
	}
	if len(tools) > 0 && a.Grant == dbstate.GrantCoordination {
		tools = append(tools, issueWriteTools...)
	}
	if p.MergeTools && slices.Contains(tools, ToolOpenPullRequest) {
		tools = append(tools, mergeTools...)
	}
	if p.DevLoop && slices.Contains(tools, ToolOpenPullRequest) {
		tools = append(tools, devLoopTools...)
	}
	// web_fetch is opt-in per agent, never by role: any live actor the owner turned it on for.
	if a.WebFetch && len(tools) > 0 {
		tools = append(tools, ToolWebFetch)
	}
	return tools
}
