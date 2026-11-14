package tasks

import (
	"context"
	"fmt"
	"strings"

	"github.com/oldsj/mainloop/server/internal/agentcontext"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
)

// DelegatedBrief is the first message a task's agent receives: the role and tool guidance, then
// the brief as the delegator wrote it, then the project's instructions section (project, "" when
// it has none), then the shared guidance for working in Mainloop. The stored brief is not
// changed; this is only what reaches the agent.
func DelegatedBrief(role dbstate.AttemptRole, mode dbstate.TaskMode, brief, project string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the Mainloop task %s. First call `whoami`, then `task_get` with its task_id.", role)
	if mode == dbstate.TaskModeCode {
		b.WriteString(" Do the work and `git push` the task branch." +
			" With `dev_sync`, try the pushed head on mainloop-dev first:" +
			" `dev_lease_acquire` until `acquired` (within 2 minutes while `queued`), then `dev_sync` that SHA, repeating while `pending`/`unknown`." +
			" Proceed only on `succeeded` with `result.health_ok` true; else fix, push, sync again." +
			" Then `dev_lease_release`; put the sync id in `evidence_refs`; re-sync after any push." +
			" Then `open_pull_request`." +
			" If CI fails a test your change does not touch but it passes locally, push one empty commit to rerun CI and say so in your report." +
			" Once CI is green, `merge_pull_request` (subject to policy); on `approval_required`, use `merge_pull_request_with_approval` or report missing approval tooling as a blocker.")
	} else {
		b.WriteString(" Do the assigned coordination work; you have no repository authority.")
	}
	b.WriteString(" Then `report` with task_id, attempt_id, outcome, evidence_refs and a stable request_id.")
	b.WriteString("\n\n## Assigned work\n")
	b.WriteString(brief)
	b.WriteString("\n\n")
	if project != "" {
		b.WriteString(project)
		b.WriteString("\n")
	}
	b.WriteString(agentcontext.Guidance)
	if mode == dbstate.TaskModeCode {
		b.WriteString("\n## Workspace environment\n" +
			"- The workspace is a gVisor sandbox: prefer fewer, larger commands.\n" +
			"- Keep `SSL_CERT_FILE`, `SSL_CERT_DIR` and `NODE_EXTRA_CA_CERTS` as set. Only allowlisted hosts are reachable; the GitHub API is not.\n" +
			"- You may run as root but cannot install OS packages.\n")
	}
	return b.String()
}

// attemptBrief renders the brief an attempt's agent receives. A task with a project carries the
// project's instructions and active memory as of the version and memory sequence pinned on the
// attempt by its first render, so a resend of the brief carries the same text.
func attemptBrief(ctx context.Context, q *dbq.Queries, a dbq.TaskAttempt, t dbq.Task) (string, error) {
	project := ""
	if t.ProjectID != nil {
		in, err := projectcontext.ForAttempt(ctx, q, a.ID, *t.ProjectID)
		if err != nil {
			return "", err
		}
		memory, err := projectcontext.MemoryForAttempt(ctx, q, a.ID, *t.ProjectID)
		if err != nil {
			return "", err
		}
		project = projectcontext.Section(in)
		if m := projectcontext.MemorySection(memory); m != "" {
			if project != "" {
				project += "\n"
			}
			project += m
		}
	}
	return DelegatedBrief(a.Role, t.Mode, t.Brief, project), nil
}
