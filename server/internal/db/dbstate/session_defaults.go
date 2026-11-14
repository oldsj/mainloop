package dbstate

// SessionDefaultRole is session_defaults.role: the main thread, or every other session.
type SessionDefaultRole string

// Session default roles.
const (
	DefaultRoleMain   SessionDefaultRole = "main"
	DefaultRoleWorker SessionDefaultRole = "worker"
)
