package tasks

import (
	"fmt"
	"strings"

	"github.com/oldsj/mainloop/server/internal/agentcontext"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// DelegatedBrief is the first message a task's agent receives: the role and tool guidance, then
// the brief as the delegator wrote it, then the shared guidance for working in Mainloop. The
// stored brief is not changed; this is only what reaches the agent.
func DelegatedBrief(role dbstate.AttemptRole, mode dbstate.TaskMode, brief string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the Mainloop task %s. First call `whoami`, then `task_get` with its task_id.", role)
	if mode == dbstate.TaskModeCode {
		b.WriteString(" Do the work and run the project's checks. `git push` the task branch." +
			" If you have `dev_sync`, try the pushed head on mainloop-staging before the PR:" +
			" `dev_lease_acquire` until `acquired` (within 2 minutes while `queued`), then `dev_sync` with that SHA." +
			" Repeat while `pending`/`unknown`. Proceed only on `succeeded` with `result.health_ok` true; else fix, push, sync again." +
			" Then `dev_lease_release`. Put the sync id in `evidence_refs`; re-sync after any push." +
			" Open the pull request with `open_pull_request` (the GitHub API is unreachable from here, so do not use gh)." +
			" If CI fails in a test your change does not touch, run that test locally; if it passes, push an empty commit once to rerun CI and say so in your report." +
			" After CI is green, call `merge_pull_request`, subject to policy." +
			" If it answers `approval_required`, call `merge_pull_request_with_approval`, or report the missing approval tooling as a blocker.")
	} else {
		b.WriteString(" Do the assigned coordination work; you have no repository authority.")
	}
	b.WriteString(" Then `report` with task_id, attempt_id, outcome, evidence_refs and a stable request_id.")
	b.WriteString("\n\n## Assigned work\n")
	b.WriteString(brief)
	b.WriteString("\n\n")
	b.WriteString(agentcontext.Guidance)
	if mode == dbstate.TaskModeCode {
		b.WriteString("\n## Workspace environment\n" +
			"- The workspace is a gVisor sandbox, so prefer fewer, larger commands.\n" +
			"- Keep `SSL_CERT_FILE`, `SSL_CERT_DIR` and `NODE_EXTRA_CA_CERTS` as they are. Only allowlisted hosts are reachable, and the GitHub API is not.\n" +
			"- You may run as root, but OS packages cannot be installed.\n")
	}
	return b.String()
}
