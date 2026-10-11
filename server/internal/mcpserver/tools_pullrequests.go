package mcpserver

// pullRequestTools is the extension point of the pull-request and merge domain:
// open_pull_request, prepare_pull_request_merge, merge_pull_request,
// merge_pull_request_with_approval and get_pull_request_merge_status. Slice G10 registers them
// here. Until then they are absent, not stubbed.
func (s *Server) pullRequestTools() []Tool { return nil }
