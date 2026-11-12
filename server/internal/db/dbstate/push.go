package dbstate

// PushState is push_notifications.state.
type PushState string

// Push notification states.
const (
	PushPending    PushState = "pending"
	PushSent       PushState = "sent"
	PushSuppressed PushState = "suppressed"
	PushFailed     PushState = "failed"
)

// PushKind is push_notifications.kind: what a notification is about.
type PushKind string

// Push notification kinds.
const (
	PushAttention PushKind = "attention"
	PushTask      PushKind = "task"
)
