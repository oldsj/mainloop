package tasks

// BriefMessageID and MessageID expose the derived ledger ids so a test can stand in for a run that
// recorded a message and then died before it noted so.
var (
	BriefMessageID     = briefMessageID
	MessageID          = messageID
	WorkspaceRequestID = workspaceRequestID
)
