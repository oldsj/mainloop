# 004 - Spawning Workers

*How Claude decides when to spawn workers and how the handoff works*

---

## The Spawn Decision

Not every task deserves a worker.

"What's the capital of France?" doesn't need a background job. Neither does "explain this function." But "add authentication to the API" or "refactor the error handling across all endpoints" — those are worker territory.

In mainloop, Claude has a tool called `spawn_task`. When you describe work, Claude evaluates whether it should:

1. **Answer directly** — Quick responses, explanations, one-off help
2. **Spawn a worker** — Sustained implementation work that runs in the background

The heuristic is roughly: **if it requires reading/writing multiple files, thinking for extended periods, or iterating until CI passes, spawn a worker.**

And crucially: Claude always asks first. "Should I spawn a worker for this?" gives you veto power. You might say "no, just tell me how I'd do it" or "yes, but also handle X."

## The Coordinator/Worker Pattern

Think of it as a human-AI management hierarchy:

```
┌─────────────────────────────────────────────────┐
│                    You                          │
│              (The Main Thread)                  │
│                                                  │
│  "Add error handling to the API"                │
└──────────────────────┬──────────────────────────┘
                       │ confirms spawn
                       ▼
┌─────────────────────────────────────────────────┐
│             Claude (Coordinator)                │
│                                                  │
│  - Interprets intent                            │
│  - Chooses appropriate worker model             │
│  - Creates task with context                    │
│  - Reports results back to you                  │
└──────────────────────┬──────────────────────────┘
                       │ spawns
                       ▼
┌─────────────────────────────────────────────────┐
│              Worker (Opus)                      │
│                                                  │
│  - Clones repo in isolated namespace            │
│  - Creates plan in GitHub issue                 │
│  - Implements in draft PR                       │
│  - Iterates until CI green                      │
│  - Reports completion                           │
└─────────────────────────────────────────────────┘
```

The coordinator (the Claude in your main conversation) is like a manager. It understands your high-level goals, translates them into concrete tasks, and delegates. Workers are like senior developers — autonomous, capable, but working on defined scope.

## The Handoff

When you confirm a spawn, here's what happens:

1. **Task created**: A WorkerTask record with:
   - The GitHub repo URL
   - The task description (what you said + coordinator's interpretation)
   - Priority and metadata

2. **Queued via DBOS**: The task enters a durable workflow queue. Even if the system restarts, the task persists.

3. **Worker pod spawns**: Kubernetes creates an isolated namespace with:
   - A fresh clone of the repo
   - Claude Code CLI with Opus model
   - Full filesystem access (within the namespace)
   - GitHub credentials for PRs and issues

4. **Execution begins**: The worker follows the plan-first workflow:
   - Creates/updates a planning issue
   - Opens a draft PR
   - Iterates until CI passes
   - Marks PR ready for review

5. **Status streams**: You see updates in your inbox:
   - "Planning started"
   - "Draft PR created: #123"
   - "CI failed, fixing..."
   - "Ready for review"

## Model Selection: Haiku vs. Opus

Not every task needs the most powerful model.

- **Haiku** (fast, cheap): Coordinator work. Understanding intent, quick responses, triaging what needs workers.
- **Opus** (powerful, slower): Worker tasks. Deep code understanding, multi-file refactors, complex implementations.

This is economical and practical. The coordinator answers in seconds. Workers take minutes to hours but produce complete, tested implementations.

## Isolation and Safety

Workers run in isolated Kubernetes namespaces. This matters for several reasons:

**Security**: A worker can't access other workers' repos or your main system. Each task is sandboxed.

**Reproducibility**: Fresh clone every time. No accumulated state or weird cache issues.

**Cleanup**: When the task completes, the namespace is deleted. No resource leakage.

**Parallelism**: Multiple workers can run simultaneously on different tasks. You can have one worker refactoring auth while another adds a new API endpoint.

## What Shows Up in Your Inbox

The inbox is your command center. For workers, you see:

1. **Active tasks**: Currently running, with expandable logs
2. **Awaiting approval**: Workers that need your input (plan review, clarification)
3. **Completed**: Recent wins, quick to review
4. **Failed**: Something broke, needs intervention

Most of the time, you're not watching workers work. You're doing other things — maybe in flow on another problem. The inbox is where workers surface when they need you.

## The Trust Gradient

Not all tasks have equal trust requirements:

- **Documentation updates**: Low stakes, maybe auto-approve
- **New features**: Medium stakes, review the plan
- **Security changes**: High stakes, review plan AND implementation
- **Production config**: Highest stakes, manual implementation

Mainloop doesn't (yet) have explicit trust levels, but the pattern emerges naturally. For risky changes, you engage more deeply. For routine work, you let workers run.

The goal is **earned autonomy**: workers that demonstrate reliability get less scrutiny over time. This is how human teams work too.

## When Workers Get Stuck

Sometimes workers fail:

- **CI won't pass**: Hit retry limit, needs human insight
- **Ambiguous requirements**: Worker asks a clarifying question
- **External blockers**: API rate limits, dependency issues

These surface in your inbox as items needing attention. You provide the input, the worker continues or a new attempt begins.

The system is designed for graceful failure. Workers don't silently fail or spin forever. They escalate to you — the main thread — when they need human judgment.

---

*Next entry: [005 - Meta Development](005-meta-development.md) — building an AI coordination system with AI assistance.*

---

**Building mainloop in public. If spawning AI workers sounds useful for your workflow, follow along.**

#buildinpublic #AI #developertools #kubernetes #architecture
