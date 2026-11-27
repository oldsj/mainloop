// Package agentcontext holds the guidance Mainloop gives every agent it starts, whatever the
// project: where the agent runs, what survives a pause, how GitHub is reached, and how work is
// reported. Project specifics belong in the repository's own AGENTS.md; what only code tasks need
// (checks, publishing, caches) is in the code brief (tasks.DelegatedBrief).
package agentcontext

// Version identifies the wording below. Bump it when the guidance changes in substance, so a
// reader of the code, tests or a stored brief can tell which guidance an agent was given.
const Version = 9

// Guidance is rendered into the first message of the main session and into every supervisor and
// child brief. It is the same for all roles and modes, so it states facts that differ by mode
// (code tasks have a checkout; the main session and coordination tasks do not) rather than by
// role, and a role-specific line never lives here. A line naming tools only code tasks have belongs
// in the code brief, not here. Keep it short: agents read it every session.
// TestGuidanceIsBounded holds the size.
const Guidance = "## Working in Mainloop (guidance v9)\n" +
	"- Where you run: sessions are isolated. Code tasks run in an isolated workspace checked out at `/data/workspace`; main and coordination tasks have none, and code work goes to code tasks.\n" +
	"- What survives a pause: committed work and `/data`. Other writes, `/tmp` and processes may be reset; run builds and tests in the foreground.\n" +
	"- Project specifics: the repository's AGENTS.md and CLAUDE.md apply.\n" +
	"- GitHub: Mainloop's tools only (no `gh`). Main and coordination tasks file issues (`create_issue`, `comment_issue`); code tasks report findings.\n" +
	"- No production commands unless the task or the owner says so.\n" +
	"- Task links: write tasks as `[<title>](/tasks/<id>)`, not bare ids.\n" +
	"- Reporting: tasks `report` evidence (commits, PRs, check output), not claims. For an owner decision, tasks report `outcome=blocked` with the question in `owner_action`; main asks the owner directly in chat.\n" +
	"- Insights: orchestration health is on the owner's `/health` page.\n" +
	"- Memory: `remember` one-line lessons for later agents: the owner's words or verified facts, not repository facts (AGENTS.md). Main's apply at once; others are proposals the owner decides. `forget` wrong ones.\n"
