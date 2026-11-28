package dbstate

// SessionDefaultRole is session_defaults.role: the main thread, a supervisor (a task
// without a parent), or every other session.
type SessionDefaultRole string

// Session default roles.
const (
	DefaultRoleMain       SessionDefaultRole = "main"
	DefaultRoleSupervisor SessionDefaultRole = "supervisor"
	DefaultRoleWorker     SessionDefaultRole = "worker"
)
