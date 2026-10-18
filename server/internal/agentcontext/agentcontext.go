// Package agentcontext holds the guidance Mainloop gives every agent it starts, whatever the
// project: where the agent runs, what survives a pause, how caches are set up, and how work gets
// published and reported. Project specifics belong in the repository's own AGENTS.md.
package agentcontext

// Version identifies the wording below. Bump it when the guidance changes in substance, so a
// reader of the code, tests or a stored brief can tell which guidance an agent was given.
const Version = 2

// Guidance is rendered into the first message of the main session and into every supervisor and
// child brief. It is the same for all roles and modes, so it states facts that differ by mode
// (code tasks have a checkout; the main session and coordination tasks do not) rather than by
// role, and a role-specific line never lives here. Keep it short: agents read it every session.
// TestGuidanceIsBounded holds the size.
const Guidance = "## Working in Mainloop (guidance v2)\n" +
	"- Where you run: Mainloop sessions are isolated. Code tasks run in an isolated workspace whose checkout is `/data/workspace`. The main session and coordination tasks have no checkout and hand code work to code tasks.\n" +
	"- What survives a pause: committed work and `/data`. Writes elsewhere, `/tmp` and processes may be reset, so commit work in progress or keep it under `/data`, and run builds and tests in the foreground.\n" +
	"- Caches: a project may use a prebuilt cache image. If it does, Go, pnpm and similar caches live under `/opt/cache`, set through environment variables such as `GOCACHE`, `GOMODCACHE` and `npm_config_store_dir`. Check the environment and use them as set; do not move them into `/data` or the repository. The first session on a new cache image can start slower.\n" +
	"- Project specifics: the repository's AGENTS.md and CLAUDE.md apply. Run the smallest checks that establish confidence, then widen them when risk warrants.\n" +
	"- Publishing: code tasks publish through Mainloop's tools (branch, pull request, CI, then merge subject to policy). Run no production commands unless the task says so. Where your tools include `dev_*`, the shared staging instance is the expected place to try a change before merge; it is not production.\n" +
	"- Reporting: tasks `report` evidence such as commits, pull requests and check output, not claims. For a decision only the owner can make, a task reports `outcome=blocked` with `owner_action` stating the question; the main session asks the owner directly in chat.\n" +
	"- Insights: the owner's Insights page (`/health`) shows orchestration health, such as tool failures and token use.\n"
