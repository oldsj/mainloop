package dbstate

// HealthCollectionState is health_collection.state. Pending is the only state the recovery sweep
// works on; collected and unavailable are final.
type HealthCollectionState string

// Health collection states.
const (
	HealthPending     HealthCollectionState = "pending"
	HealthCollected   HealthCollectionState = "collected"
	HealthUnavailable HealthCollectionState = "unavailable"
)
