# Mainloop roadmap

Status: proposed product direction. Repository specifications and code describe implemented behavior until each roadmap phase updates them.

## Vision

Mainloop is a conversation-centered workspace for managing multiple projects and AI coding-agent sessions without losing context.

The core experience is straightforward:

1. Start or continue work from a phone or desktop.
2. Capture another idea without disrupting the current task.
3. See which projects and sessions are active or need attention.
4. Return to earlier work with its decisions, current state, and next action intact.

## Product principles

- **Continuity over transcript replay.** Return to current project state instead of rereading an entire conversation.
- **Native agents.** Integrate established agent runtimes rather than implementing another model loop.
- **Durable control state.** Messages, tasks, bindings, receipts, attention, and checkpoints survive worker restarts.
- **Explicit delivery.** Distinguish recorded, queued, delivered, completed, and failed work.
- **Visible execution.** Every running agent and workspace is discoverable and directly addressable.
- **Bounded autonomy.** Work proceeds only within explicit scope, resource limits, and recovery limits.
- **Cohesive parallelism.** Increase concurrency only when shared contracts and integration ownership remain clear.
- **Portable data.** Notes, decisions, checkpoints, and artifacts remain exportable in common formats.

## Product experience

| View     | Purpose                                                                              |
| -------- | ------------------------------------------------------------------------------------ |
| Home     | Current conversation, selected focus, meaningful updates, and requests for attention |
| Projects | Project goals, tasks, decisions, notes, artifacts, checkpoints, and next actions     |
| Sessions | Agent sessions grouped by project and task, with conversation and terminal access    |
| Capacity | Provider availability, usage signals, queues, workspace resources, and trends        |

Mobile supports messaging, answering questions, approving decisions, reading results, steering work, and switching projects without terminal navigation. Desktop adds denser project, session, evidence, and workspace views.

Project state and runtime state remain separate. An idle session is not necessarily complete, and a proposed next action is not necessarily authorized work. Repeated unchanged blockers produce one persistent attention item rather than repeated notifications.

## Architecture

```text
Mobile and desktop clients
            |
            v
Mainloop control plane
  conversation, projects, tasks, policy, delivery, attention, evidence
            |
            v
Deterministic scheduler and workspace control
            |
            v
Workspace runtime adapters
            |
            v
Native agents, repositories, tools, and development services
```

The control plane and worker workspaces have distinct responsibilities:

- The control plane owns durable product state, task authority, routing, message delivery, attention, and audit records.
- A workspace runtime owns native-agent process control and reports observable lifecycle events.
- Native runtimes retain their own conversation identity, tools, compaction, and provider-specific behavior.
- PostgreSQL stores authoritative product state. Git remains authoritative for source revisions.
- Append-only event and log storage preserves evidence outside ephemeral processes.

The workspace runtime is not an inference proxy. It does not rebuild model prompts, interpret provider tool calls, or replace a native agent's conversation loop.

The workspace runtime is kagent. Mainloop is an A2A client of the kagent gateway: each native session binding maps to one kagent Session, and Mainloop records every message in its delivery ledger before sending it. kagent owns agent pods, native harness processes, workspace bootstrap, snapshots and session expiry. Mainloop does not run a second scheduler or session owner alongside it.

### Implementation direction (implemented)

The Go/React migration is done. The cutover ([#191](https://github.com/oldsj/mainloop/pull/191), 2026-10-12) made `main` build the Go server and the React app. The Python backend, the SvelteKit frontend and DBOS are removed. [docs/architecture.md](docs/architecture.md) describes the current implementation.

The implemented stack keeps the control-plane model:

- **Backend.** A Go service built as one binary. PostgreSQL holds the durable product state. Background work runs as a Postgres reconciler: each side effect is first written as an intent row with an explicit state and an idempotency key, workers claim rows and commit before any network I/O, and an uncertain outcome is resolved by observing the remote side before any retry. No database lock or transaction is held across network I/O.
- **Kubernetes.** No operator, no custom resources, and no etcd-backed coordination. kagent and GitOps own workload resources. Mainloop's in-cluster writes are limited to the runtime credential Secrets it already creates.
- **Contract.** `api/openapi.yaml` is the wire contract. Go and TypeScript types are generated from it, and contract changes come before implementation changes.
- **Frontend.** A React single-page app that calls the API through the generated client. It replaces the SvelteKit app.

## Context and continuity

The visible conversation may remain continuous while working context follows the active topic.

1. Persist each message and associate it with a topic or project.
2. Store decisions, constraints, unresolved questions, and requested actions as source-linked records.
3. Track delegated work through explicit delivery and acceptance states.
4. Preserve pending intent before compaction, continuation, or topic switching.
5. Assemble context from standing project information, the current topic, its checkpoint, and selected evidence.
6. Resume from current durable state instead of replaying every intervening message.

Summaries are derived and versioned. They do not grant authority or turn an agent's success claim into verified completion. Checkpoints update at meaningful transitions and remain readable without invoking a model.

## Coordination and cohesion

Simple tasks do not require a supervisor hierarchy. Larger projects use supervisors only where a real coordination boundary exists.

A supervisor owns a bounded cohesion domain: work whose shared contracts, conventions, dependencies, and integration state can reasonably remain in context. Before parallel work begins, the supervisor:

- identifies existing helpers and cross-cutting concerns;
- resolves or assigns shared contracts;
- sequences foundation work ahead of dependent work;
- names the integration owner; and
- separates work that can proceed independently from work that must remain serial.

Parallel workers must not silently introduce competing implementations of shared concerns such as authentication, cryptography, persistence, error handling, API types, or UI primitives.

Fan-out is limited by cohesion as well as compute and model capacity. When a supervisor can no longer track active candidates, decisions, dependencies, and integration risks, the batch stops growing or splits along stable subsystem boundaries.

## Tasks, goals and agent context

Status: proposed direction, not implemented.

Agents in Mainloop currently receive less context than prior native-harness workflows, task state is split between the database and external note files, and there is no way to see where a running supervisor stands in its work without opening it.

### Agent context

- Claude Code reads `CLAUDE.md`, not `AGENTS.md`. Repositories keep `AGENTS.md` as the single source and add a `CLAUDE.md` that imports it (`@AGENTS.md`), so Claude and Codex agents load the same nested project instructions natively.
- An owner-level instruction document, stored in Mainloop and rendered into every main, supervisor, and child brief, would carry communication style, evidence rules, no-hidden-subagents, and review expectations shared across projects. Bounded in size, versioned, and visible in the UI.
- Briefs should state what the agent is — its role, that it runs in an isolated workspace, which tools it has, and what it may not do — rather than adding another prompt layer. This extends the existing brief construction instead of layering on top of it.

### Tasks as the native unit

- Mainloop's durable tasks become the source of truth for work: a tree of goal, supervisor, and children, each with status, plan, decisions, reports, PRs, and evidence. External note files stop being authoritative for task state.
- An explorer view shows each parent task's progress without opening it: children done, running, blocked, or waiting on the owner, and the latest report line.
- A task page shows the plan and decision log as structured records (see [Context and continuity](#context-and-continuity) items 2–3), not free text only.

### Export, not sync

- Mainloop can export tasks as Markdown, one folder per task with frontmatter carrying status and links, into a git repository that external note tools may render. Export only: edits flow back through Mainloop, never by syncing files back in.

### Goals with guardrails

- A goal is a long-running supervisor task with explicit limits set at creation: token or quota budget, wall-clock budget, maximum concurrent children, maximum repair attempts per child, allowed projects and paths, and decisions reserved for the owner that raise approval cards. This extends the bounded-autonomy direction already set out for [Phase 2](#phase-2-cohesive-queues-and-bounded-autonomy).
- A goal has a "done when" condition the supervisor must prove with evidence — a merged PR, a live check — rather than claim. It stops and asks the owner when a limit is reached, when repairs stop converging, or when a reserved decision comes up.
- Goals share the same task tree and explorer progress as other work.

### Slices

In rough order: agent context docs (this section, plus a repository `CLAUDE.md`); explorer progress showing child state counts and the latest report line per parent task, reusing existing task reads without new server API where a count can be derived; a task-steering tool for sending messages into running tasks; the owner policy document rendered into briefs; goal limits and done-when conditions on supervisor tasks; Markdown export and one-time import of prior note archives.

## Scheduling and capacity

Work moves through explicit states such as proposed, planned, ready, running, waiting, held, accepted, and cancelled. Dependency-blocked work is not ready.

The scheduler first filters by authority, dependencies, ownership, required capabilities, review independence, and available capacity. It may then rank eligible work using priority, urgency, estimated time to acceptance, queue delay, provider availability, and resource headroom.

Capacity reporting keeps distinct signals separate:

- provider-reported availability and usage windows;
- observed input, output, and cached-token activity where available;
- activity attributable to a project, task, stage, or session;
- workspace CPU, memory, storage, and queue time; and
- unavailable or unattributed measurements.

Mainloop does not invent conversions between tokens, subscription limits, monetary cost, or remaining tasks. Routing profiles are evaluated by time and usage through accepted work, including checks, review, and repair.

Proposed [orchestration health](docs/specs/orchestration-health.md) adds privacy-safe native
telemetry for friction, repeated file reads, tokens/cache and compaction, with per-task and
per-agent rollups that expose missing measurements. Start with Claude tool failures and
input/output counts on one page. Follow with model/effort fit against labeled difficulty and
outcomes, subscription limits/reset windows with clearly sourced headroom or estimates, and
scheduled merged-diff reviews for design-cohesion findings. These views inform owner steering;
they do not authorize provider switches or another scheduler.

## Delivery, recovery, and review

Messages receive stable identities and are persisted before delivery. Retries with the same identity do not create a second logical instruction.

After a timeout or disconnect, Mainloop reconciles native-session and workspace state before deciding whether to retry. Unknown delivery is not equivalent to failure. Recovery never replays transcript commands automatically.

Production delivery is mechanical: a merge to `main` publishes images, and CI force-pushes a `release` branch carrying their digests, which Argo tracks. Rollback is pushing a previous release commit back to `release`.

Check results and review verdicts bind to an exact candidate identity. Integration review covers combined-system concerns such as duplicated foundations, conflicting abstractions, contract drift, and inconsistent security or data handling—not only whether each feature works independently.

Failure handling distinguishes provider availability, transport errors, source defects, integration failures, and missing authority. Recovery and repair use finite limits and stop on repeated no-progress outcomes.

## Workspace platform

The target execution platform provides one isolated writable workspace per concurrent writer, with persistent source and native-session state, bounded resources, scoped development services, preview endpoints, and observable lifecycle state. Reviewers may receive a read-only frozen candidate. On kagent, a workspace is a Session with a Git bootstrap and snapshot-on-quiesce; previews reach it through the kagent router and idle out by suspending the Session.

Workspace identity does not depend on a process or pod name. Replacing workspace compute must preserve acknowledged product state and expose any gap in native or log recovery.

The control plane remains outside worker isolation boundaries. Workspace provisioning has one authoritative resource owner and integrates with declarative deployment workflows rather than competing with them.

Security evolves with deployment scope. Initial milestones establish correct isolation and delivery semantics; later milestones add stronger credential scoping, egress control, runtime hardening, retention controls, and audit capabilities.

## Delivery phases

### Phase 0: native integration proof

Establish a minimal, evidence-backed adapter contract for supported native agents. Prove session discovery or creation, message delivery, completion, attention, identity, usage visibility, history, reconnect, interruption, and context continuation where each runtime supports them.

Exit criteria:

- capability results distinguish proved, partial, unsupported, and unknown behavior;
- duplicate submission and reconnect do not duplicate logical work;
- uncertain delivery is reconciled rather than blindly replayed;
- checkpoints remain readable while agents are stopped; and
- default automated tests use recorded fixtures rather than live provider calls.

### Phase 1: useful continuity

Implement durable projects, topics, tasks, session bindings, conversation, checkpoints, notes, attention, queued delivery, and responsive project/session views.

Exit criteria:

- switching projects preserves pending intent and current state;
- reconnect and duplicate submission do not duplicate work;
- a question or approval reaches the correct session; and
- returning to a task requires no terminal navigation.

### Phase 2: cohesive queues and bounded autonomy

Add cohesion-limited supervisors, deterministic scheduling, routing preferences and requirements, capacity-aware admission, local runners, typed failure handling, and bounded recovery.

Exit criteria:

- multiple projects progress under shared capacity limits;
- workers reuse declared shared foundations;
- proposed contract changes return to the owning supervisor;
- integration review catches duplicate abstractions; and
- fallback cannot create two uncertain writers for one task.

### Phase 3: workspace lifecycle and off-host recovery

Add production workspace provisioning, persistent state, authenticated runtime channels, previews, quotas, monitoring, off-workspace logs, and backup restoration.

Exit criteria:

- concurrent workspaces respect isolation boundaries;
- quotas contain resource exhaustion;
- replacing workspace compute preserves acknowledged records; and
- recovery exposes rather than hides unsynchronized state.

### Phase 4: operational completeness

Finish capacity trends, per-task and per-session accounting, searchable evidence, artifact views, checkpoint navigation, data import/export, and lifecycle management.

Exit criteria:

- projects can be operated from mobile and desktop clients;
- usage displays distinguish measured, attributed, and unknown activity;
- exported project context remains usable outside Mainloop; and
- retained data follows explicit lifecycle rules.

## Future opportunities

- Voice capture and interaction.
- Additional native-agent and local-runner adapters.
- Policy-as-code for workspace capabilities and external actions.
- Desktop interaction for tasks that cannot be automated through command-line or API interfaces.
- Automated CI failure analysis and repair within bounded task authority.
- Reusable project and workspace templates.

## Open design questions

- Which native interfaces provide reliable completion, attention, usage, history, and continuation signals?
- What is the smallest workspace-runtime protocol that supports delivery, events, reconnect, and ownership?
- How should context budgets, cohesion limits, interactive reserve, and recovery limits be calibrated?
- Which local runners meet tool-use, context, identity, completion, and recovery requirements?
- What retention and restoration guarantees should apply to messages, logs, artifacts, and native state?
- Which workspace isolation and credential models are appropriate for different deployment profiles?
- How do agents call Mainloop tools (delegate, note, report, approvals) with per-session identity, without a credential inside the agent?
