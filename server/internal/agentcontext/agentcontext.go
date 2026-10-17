// Package agentcontext holds the guidance Mainloop gives every agent it starts, whatever the
// project: where the agent runs, what survives a pause, how caches are set up, and how work gets
// published and reported. Project specifics belong in the repository's own AGENTS.md.
package agentcontext

// Version identifies the wording below. Bump it when the guidance changes in substance, so a
// reader of the code, tests or a stored brief can tell which guidance an agent was given.
const Version = 1

// Guidance is rendered into the first message of the main session and into every supervisor and
// child brief. It is the same for all roles, so a role-specific line never lives here. Keep it
// short: agents read it every session. TestGuidanceIsBounded holds the size.
const Guidance = "## Working in Mainloop (guidance v1)\n" +
	"- Where you run: Mainloop sessions are isolated. Code work happens in an isolated workspace whose checkout is `/data/workspace`; main and supervisor sessions have no checkout and delegate code work to child tasks.\n" +
	"- What survives a pause: committed work and `/data`. Writes elsewhere, `/tmp` and processes may be reset, so commit work in progress or keep it under `/data`, and run builds and tests in the foreground.\n" +
	"- Caches: a project may use a prebuilt cache image. If it does, Go, pnpm and similar caches live under `/opt/cache`, set through environment variables such as `GOCACHE`, `GOMODCACHE` and `npm_config_store_dir`. Check the environment and use them as set; do not move them into `/data` or the repository. The first session on a new cache image can start slower.\n" +
	"- Project specifics: the repository's AGENTS.md and CLAUDE.md apply. Run the smallest checks that establish confidence, then widen them when risk warrants.\n" +
	"- Publishing: code tasks publish through Mainloop's tools (branch, pull request, CI, then merge subject to policy). Run no production or live commands unless the task says so.\n" +
	"- Reporting: `report` evidence such as commits, pull requests and check output, not claims. For a decision only the owner can make, report `outcome=blocked` with `owner_action` stating the question.\n" +
	"- Insights: the owner's Insights page (`/health`) shows orchestration health, such as tool failures and token use.\n"
