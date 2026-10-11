package mcpserver

// taskTools is the extension point of the task domain: delegate, report, task_get, task_list,
// task_history and task_cancel. Slice G8a registers them here. Until then the tools are absent,
// not stubbed: the policy table names them but tools/list only lists what is registered.
func (s *Server) taskTools() []Tool { return nil }
