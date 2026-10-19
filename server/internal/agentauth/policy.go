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
	ToolListDependabotAlerts      = "list_dependabot_alerts"
	ToolTaskGet                   = "task_get"
	ToolTaskList                  = "task_list"
	ToolTaskHistory               = "task_history"
	ToolDelegate                  = "delegate"
	ToolTaskCancel                = "task_cancel"
	ToolReport                    = "report"
	ToolPreparePullRequestMerge   = "prepare_pull_request_merge"
	ToolMergePullRequest          = "merge_pull_request"
	ToolMergePullRequestApproval  = "merge_pull_request_with_approval"
	ToolGetPullRequestMergeStatus = "get_pull_request_merge_status"
	ToolDevLeaseAcquire           = "dev_lease_acquire"
	ToolDevLeaseRelease           = "dev_lease_release"
	ToolDevSync                   = "dev_sync"
	ToolDevStatus                 = "dev_status"
)

// AllTools lists every tool, in the order api/mcp-tools.json lists them.
var AllTools = []string{
	ToolWhoami, ToolTopics, ToolTopicOpen, ToolNote, ToolDecide, ToolPendingAdd, ToolPendingDone,
	ToolOpenPullRequest,
	ToolGetIssue, ToolListDependabotAlerts,
	ToolTaskGet, ToolTaskList, ToolTaskHistory, ToolDelegate, ToolTaskCancel, ToolReport,
	ToolPreparePullRequestMerge, ToolMergePullRequest, ToolMergePullRequestApproval,
	ToolGetPullRequestMergeStatus,
	ToolDevLeaseAcquire, ToolDevLeaseRelease, ToolDevSync, ToolDevStatus,
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
	taskRead       = []string{ToolTaskGet, ToolTaskList, ToolTaskHistory}
	taskManagement = []string{ToolDelegate, ToolTaskCancel}
	workspaceTools = []string{ToolWhoami, ToolOpenPullRequest}
	mergeTools     = []string{
		ToolPreparePullRequestMerge, ToolMergePullRequest, ToolMergePullRequestApproval,
		ToolGetPullRequestMergeStatus,
	}
	devLoopTools = []string{
		ToolDevLeaseAcquire, ToolDevLeaseRelease, ToolDevSync, ToolDevStatus,
	}
	// issueTools are the read-only GitHub issue and Dependabot-alert tools. They carry no feature
	// gate of their own: wherever an actor can open a pull request on its bound project, it can
	// also read that project's issues and alerts.
	issueTools = []string{ToolGetIssue, ToolListDependabotAlerts}
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
	case (a.Role == dbstate.RoleSupervisor && a.Depth == 1) || (a.Role == dbstate.RoleChild && a.Depth == 2):
		tools = append(tools, ToolWhoami, ToolReport)
		tools = append(tools, taskRead...)
		if a.Role == dbstate.RoleSupervisor {
			tools = append(tools, taskManagement...)
		}
		if a.Grant == dbstate.GrantWorkspace {
			tools = append(tools, workspaceTools...)
		}
	case a.Role == dbstate.RoleAgent && a.Depth == 0:
		tools = append(tools, workspaceTools...)
	}
	if slices.Contains(tools, ToolOpenPullRequest) {
		tools = append(tools, issueTools...)
	}
	if p.MergeTools && slices.Contains(tools, ToolOpenPullRequest) {
		tools = append(tools, mergeTools...)
	}
	if p.DevLoop && slices.Contains(tools, ToolOpenPullRequest) {
		tools = append(tools, devLoopTools...)
	}
	return tools
}
