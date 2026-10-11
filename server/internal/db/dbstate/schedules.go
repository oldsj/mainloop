package dbstate

// ScheduleState is scheduled_run_schedules.state. A row is an intent from before the create is sent
// until kagent answers it; the SQL that moves it holds the literals.
type ScheduleState string

// Scheduled-run registry states.
const (
	ScheduleIntent  ScheduleState = "intent"
	ScheduleCreated ScheduleState = "created"
)
