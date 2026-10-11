package dbstate

// DeliveryState is deliveries.state. Recorded, sending and delivered are the open states: at
// most one delivery per session is in one of them.
type DeliveryState string

// Delivery states.
const (
	DeliveryRecorded  DeliveryState = "recorded"
	DeliverySending   DeliveryState = "sending"
	DeliveryDelivered DeliveryState = "delivered"
	DeliveryCompleted DeliveryState = "completed"
	DeliveryFailed    DeliveryState = "failed"
	DeliveryCancelled DeliveryState = "cancelled"
	DeliveryUncertain DeliveryState = "uncertain"
)
