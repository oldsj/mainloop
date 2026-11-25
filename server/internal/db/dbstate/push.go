package dbstate

// PushState is push_notifications.state and apns_deliveries.state.
type PushState string

// Push notification states.
const (
	PushPending    PushState = "pending"
	PushSent       PushState = "sent"
	PushSuppressed PushState = "suppressed"
	PushFailed     PushState = "failed"
	// PushSkipped is a push_notifications row no ntfy channel will send (ntfy is off).
	PushSkipped PushState = "skipped"
)

// PushKind is push_notifications.kind: what a notification is about.
type PushKind string

// Push notification kinds.
const (
	PushAttention PushKind = "attention"
	PushTask      PushKind = "task"
)

// APNsEnvironment is device_push_tokens.environment: the APNs environment an app build was signed
// for (its aps-environment entitlement), which picks Apple's host.
type APNsEnvironment string

// APNs environments.
const (
	APNsDevelopment APNsEnvironment = "development"
	APNsProduction  APNsEnvironment = "production"
)
