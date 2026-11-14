// Package agentcontext holds the guidance Mainloop gives every agent it starts, whatever the
// project: where the agent runs, what survives a pause, how caches are set up, and how work gets
// published and reported. Project specifics belong in the repository's own AGENTS.md.
package agentcontext

// Version identifies the wording below. Bump it when the guidance changes in substance, so a
// reader of the code, tests or a stored brief can tell which guidance an agent was given.
const Version = 5

// Guidance is rendered into the first message of the main session and into every supervisor and
// child brief. It is the same for all roles and modes, so it states facts that differ by mode
// (code tasks have a checkout; the main session and coordination tasks do not) rather than by
// role, and a role-specific line never lives here. Keep it short: agents read it every session.
// TestGuidanceIsBounded holds the size.
const Guidance = "## Working in Mainloop (guidance v5)\n" +
	"- Where you run: sessions are isolated. Code tasks run in an isolated workspace checked out at `/data/workspace`; the main session and coordination tasks have none and delegate code work.\n" +
	"- What survives a pause: committed work and `/data`. Other writes, `/tmp` and processes may be reset, so commit or keep work in progress under `/data`; run builds and tests in the foreground.\n" +
	"- Caches: with a cache image, Go, pnpm and other caches live under `/opt/cache` (`GOCACHE`, `GOMODCACHE`, `pnpm_config_store_dir`); use them as set; do not move them.\n" +
	"- Project specifics: the repository's AGENTS.md and CLAUDE.md apply.\n" +
	"- Checks: run what `get_verification` says; CI runs the rest. Push once, call `watch_checks` (`reviews` on a pull request) and end your turn; a message brings results or reviews. After three failed rounds on one check or review, report `outcome=blocked`. No CI: run all checks and propose a starter workflow.\n" +
	"- Publishing: code tasks publish through Mainloop's tools (branch, pull request, CI, merge subject to policy). No production commands unless the task says so. With `dev_*` tools, try changes on the shared dev instance before merge.\n" +
	"- GitHub: Mainloop's tools only (no `gh`); ask the owner to file issues.\n" +
	"- Task links: write tasks as `[<title>](/tasks/<id>)`, not bare ids.\n" +
	"- Reporting: tasks `report` evidence (commits, PRs, check output), not claims. For an owner-only decision, tasks report `outcome=blocked` with the question in `owner_action`; the main session asks the owner directly in chat.\n" +
	"- Insights: the owner's Insights page (`/health`) shows orchestration health.\n" +
	"- Memory: `remember` one-line lessons for later agents: the owner's words or verified facts, not repository facts (AGENTS.md). Main's apply at once; others are proposals the owner decides. `forget` wrong ones.\n"
