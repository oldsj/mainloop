package dbstate

// DeliveryState is deliveries.state. Recorded, sending and delivered are the open states: at
// most one delivery per session is in one of them. Queued is deliberately excluded from that rule:
// many messages may queue behind the open one, in the order they arrived, and are admitted one at a
// time as each turn ends.
type DeliveryState string

// Delivery states.
const (
	DeliveryQueued    DeliveryState = "queued"
	DeliveryRecorded  DeliveryState = "recorded"
	DeliverySending   DeliveryState = "sending"
	DeliveryDelivered DeliveryState = "delivered"
	DeliveryCompleted DeliveryState = "completed"
	DeliveryFailed    DeliveryState = "failed"
	DeliveryCancelled DeliveryState = "cancelled"
	DeliveryUncertain DeliveryState = "uncertain"
)

// RestartState is session_restarts.state: requested until the native Session is confirmed gone and
// the turns on it are settled, then done.
type RestartState string

// Restart states.
const (
	RestartRequested RestartState = "requested"
	RestartDone      RestartState = "done"
)
