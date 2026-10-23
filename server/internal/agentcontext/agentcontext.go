// Package agentcontext holds the guidance Mainloop gives every agent it starts, whatever the
// project: where the agent runs, what survives a pause, how caches are set up, and how work gets
// published and reported. Project specifics belong in the repository's own AGENTS.md.
package agentcontext

// Version identifies the wording below. Bump it when the guidance changes in substance, so a
// reader of the code, tests or a stored brief can tell which guidance an agent was given.
const Version = 4

// Guidance is rendered into the first message of the main session and into every supervisor and
// child brief. It is the same for all roles and modes, so it states facts that differ by mode
// (code tasks have a checkout; the main session and coordination tasks do not) rather than by
// role, and a role-specific line never lives here. Keep it short: agents read it every session.
// TestGuidanceIsBounded holds the size.
const Guidance = "## Working in Mainloop (guidance v4)\n" +
	"- Where you run: sessions are isolated. Code tasks run in an isolated workspace checked out at `/data/workspace`; the main session and coordination tasks have no checkout and hand code work to code tasks.\n" +
	"- What survives a pause: committed work and `/data`. Other writes, `/tmp` and processes may be reset: commit work in progress or keep it under `/data`, and run builds and tests in the foreground.\n" +
	"- Caches: with a prebuilt cache image, Go, pnpm and similar caches live under `/opt/cache` (`GOCACHE`, `GOMODCACHE`, `npm_config_store_dir`). Use them as set; do not move them into `/data` or the repository. A new image's first session can be slower.\n" +
	"- Project specifics: the repository's AGENTS.md and CLAUDE.md apply. Run the smallest checks that establish confidence; widen them as risk warrants.\n" +
	"- Publishing: code tasks publish through Mainloop's tools (branch, pull request, CI, then merge subject to policy). Run no production commands unless the task says so. Where your tools include `dev_*`, the shared dev instance (not production) is where to try a change before merge.\n" +
	"- GitHub: use Mainloop's tools only; there is no `gh` or issue tool yet, so ask the owner to file issues.\n" +
	"- Task links: write tasks as `[<title>](/tasks/<id>)`, not bare ids.\n" +
	"- Reporting: tasks `report` evidence (commits, pull requests, check output), not claims. For an owner-only decision, a task reports `outcome=blocked` with `owner_action` stating the question; the main session asks the owner directly in chat.\n" +
	"- Insights: the owner's Insights page (`/health`) shows orchestration health: tool failures, token use.\n"
