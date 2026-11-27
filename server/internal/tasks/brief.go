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
		b.WriteString(" Do the work, run what `get_verification` says (CI runs the rest), then `git push` the task branch: push once, not after every commit." +
			" With `dev_sync`, try the pushed head on the project's staging target first: hold the lease (`dev_lease_acquire`), `dev_sync` that SHA until it settles," +
			" and go on only on `succeeded` with `result.health_ok` true, else fix, push and sync again; then `dev_lease_release`, put the sync id in `evidence_refs`, and re-sync after any push." +
			" Then `open_pull_request`, call `watch_checks` with `reviews` and end your turn. After three failed rounds on one check or review, report `outcome=blocked`." +
			" If CI fails a test your change does not touch but it passes locally, push one empty commit to rerun CI and say so in your report." +
			" With no CI, run every check and propose a starter workflow." +
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
			"- The workspace is a sandbox: prefer fewer, larger commands.\n" +
			"- Keep `SSL_CERT_FILE`, `SSL_CERT_DIR` and `NODE_EXTRA_CA_CERTS` as set; only allowlisted hosts are reachable.\n" +
			"- With a cache image, caches live under `/opt/cache` (`GOCACHE`, `GOMODCACHE`, `pnpm_config_store_dir`); use them as set and do not move them.\n" +
			"- You may run as root but cannot install OS packages.\n")
	}
	return b.String()
}

// StandingGuidance follows a standing supervisor's brief on every attempt: what it is and how it
// works (docs/specs/project-supervisor.md).
const StandingGuidance = "## Standing supervisor\n" +
	"You are this project's standing supervisor, the middle of three layers: main sends you work and you report to main; " +
	"the workers and reviewers you `delegate` report to you and cannot delegate.\n" +
	"- You never complete. When a batch of work is done, `report` outcome `completed` with a summary and end your turn; " +
	"the next message wakes you. Report `blocked`, never `failed`.\n" +
	"- Hand code to workers (`delegate` mode `code`) and manage them with the task tools; arm a loop or end your turn rather than poll.\n"

// attemptBrief renders the brief an attempt's agent receives. A task with a project carries the
// project's instructions and active memory as of the version and memory sequence pinned on the
// attempt by its first render, so a resend of the brief carries the same text. A retry's or
// reassign's attempt also gets a continuation note after the brief; see [continuation].
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
	brief := t.Brief
	if t.Standing {
		brief += "\n\n" + StandingGuidance
	}
	if note, err := continuation(ctx, q, a, t); err != nil {
		return "", err
	} else if note != "" {
		brief += "\n\n" + note
	}
	return DelegatedBrief(a.Role, t.Mode, brief, project), nil
}
