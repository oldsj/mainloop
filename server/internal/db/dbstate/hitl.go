package dbstate

// HITLKind is hitl_requests.kind.
type HITLKind string

// HITL request kinds.
const (
	HITLToolApproval HITLKind = "tool_approval"
	HITLAskUser      HITLKind = "ask_user"
)

// HITLAvailability is hitl_requests.availability. Pending and stale are the live states: a task has
// at most one request in either.
type HITLAvailability string

// HITL request availabilities.
const (
	HITLPending     HITLAvailability = "pending"
	HITLStale       HITLAvailability = "stale"
	HITLUnavailable HITLAvailability = "unavailable"
	HITLAnswered    HITLAvailability = "answered"
)

// HITLResponseState is hitl_responses.state. Recorded, sending, accepted and uncertain are the live
// states: a request has at most one decision in any of them.
type HITLResponseState string

// HITL response states.
const (
	HITLRecorded          HITLResponseState = "recorded"
	HITLSending           HITLResponseState = "sending"
	HITLAccepted          HITLResponseState = "accepted"
	HITLUncertain         HITLResponseState = "uncertain"
	HITLRejectedTransport HITLResponseState = "rejected_transport"
)

// AttentionKind is attention_items.kind, the contract's AttentionItem kind.
type AttentionKind string

// Attention kinds. Approval, question and owner_decision are real owner decisions; problem is
// Mainloop's own recovery work with no decision yet for the owner to make.
const (
	AttentionApproval      AttentionKind = "approval"
	AttentionQuestion      AttentionKind = "question"
	AttentionOwnerDecision AttentionKind = "owner_decision"
	AttentionProblem       AttentionKind = "problem"
)

// AttentionState is attention_items.state.
type AttentionState string

// Attention states.
const (
	AttentionOpen      AttentionState = "open"
	AttentionResolved  AttentionState = "resolved"
	AttentionDismissed AttentionState = "dismissed"
)

// SourceHITLRequest is the attention_items.source_kind of an item that stands for a HITL request.
const SourceHITLRequest = "hitl_request"
