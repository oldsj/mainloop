package dbstate

// PRCreationState is pr_creations.state. Pending, dispatching and uncertain are the states a loop
// works on; created and refused are final.
type PRCreationState string

// Pull request creation states.
const (
	PRPending     PRCreationState = "pending"
	PRDispatching PRCreationState = "dispatching"
	PRUncertain   PRCreationState = "uncertain"
	PRCreated     PRCreationState = "created"
	PRRefused     PRCreationState = "refused"
)

// MergeState is merge_candidates.state. Evaluating, merging and uncertain are the states a loop
// works on; blocked, expired, superseded and merged are final.
type MergeState string

// Merge candidate states.
const (
	MergePrepared   MergeState = "prepared"
	MergeEvaluating MergeState = "evaluating"
	MergeMerging    MergeState = "merging"
	MergeUncertain  MergeState = "uncertain"
	MergeBlocked    MergeState = "blocked"
	MergeExpired    MergeState = "expired"
	MergeSuperseded MergeState = "superseded"
	MergeMerged     MergeState = "merged"
)

// MergeRoute is merge_candidates.route: who has to say yes.
type MergeRoute string

// Merge routes. They share the project policy's two values.
const (
	RouteAuto     MergeRoute = "auto"
	RouteApproval MergeRoute = "approval"
)

// MergeTool is merge_request_keys.tool.
type MergeTool string

// Merge tools that carry a request id.
const (
	ToolPrepare       MergeTool = "prepare"
	ToolMerge         MergeTool = "merge"
	ToolApprovedMerge MergeTool = "approved_merge"
)
