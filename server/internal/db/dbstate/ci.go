package dbstate

// CheckWatchState is check_watches.state: an agent's request to hear when CI on one commit
// settles. Watching and delivering are claimable; the rest are final.
type CheckWatchState string

// Check watch states.
const (
	WatchWatching      CheckWatchState = "watching"
	WatchDelivering    CheckWatchState = "delivering"
	WatchDelivered     CheckWatchState = "delivered"
	WatchUndeliverable CheckWatchState = "undeliverable"
	WatchSuperseded    CheckWatchState = "superseded"
)

// CheckWatchOutcome is check_watches.outcome: how the watched CI settled.
type CheckWatchOutcome string

// Check watch outcomes.
const (
	WatchGreen          CheckWatchOutcome = "green"
	WatchFailed         CheckWatchOutcome = "failed"
	WatchPendingTimeout CheckWatchOutcome = "pending_timeout"
	WatchNoChecks       CheckWatchOutcome = "no_checks"
	WatchNotFound       CheckWatchOutcome = "not_found"
)
