package config

// Sessions tunes the native sessions' delivery ledger.
type Sessions struct {
	// UncertainHold is how long a delivery whose send may have reached kagent keeps blocking new
	// sends while it is looked up. After it, a message that no kagent task shows is marked failed.
	UncertainHold Seconds `env:"SESSION_UNCERTAIN_HOLD_SECONDS" envDefault:"1800"`
	// UncertainRecheck is how often an unresolved delivery is looked up again.
	UncertainRecheck Seconds `env:"SESSION_UNCERTAIN_RECHECK_SECONDS" envDefault:"30"`
	// FollowPoll is how often a running task is read again once its event stream has ended.
	FollowPoll Seconds `env:"SESSION_FOLLOW_POLL_SECONDS" envDefault:"2"`
	// ParkedMax is the longest wait between reads of a task parked for an answer (input or auth
	// required). The wait grows from FollowPoll up to this.
	ParkedMax Seconds `env:"SESSION_PARKED_MAX_SECONDS" envDefault:"300"`
	// ReadyWait is how long a send waits for a starting or resuming kagent Session to be ready.
	ReadyWait Seconds `env:"SESSION_READY_WAIT_SECONDS" envDefault:"60"`
	// DeliveryConcurrency is how many deliveries are handled at once. A handler follows a turn to
	// its end, so this is the number of turns that can run at the same time.
	DeliveryConcurrency int `env:"SESSION_DELIVERY_CONCURRENCY" envDefault:"16"`
	// EnsureEvery is how often the main session is checked for.
	EnsureEvery Seconds `env:"SESSION_ENSURE_SECONDS" envDefault:"60"`
	// CarryOverMessages is how many recent completed user and assistant messages the first
	// message to a new main Session carries from the earlier conversation. 0 carries none; at most 100.
	CarryOverMessages int `env:"MAIN_CARRY_OVER_MESSAGES" envDefault:"6"`
}
