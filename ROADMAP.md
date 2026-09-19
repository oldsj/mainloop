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

| View | Purpose |
| --- | --- |
| Home | Current conversation, selected focus, meaningful updates, and requests for attention |
| Projects | Project goals, tasks, decisions, notes, artifacts, checkpoints, and next actions |
| Sessions | Agent sessions grouped by project and task, with conversation and terminal access |
| Capacity | Provider availability, usage signals, queues, workspace resources, and trends |

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

## Delivery, recovery, and review

Messages receive stable identities and are persisted before delivery. Retries with the same identity do not create a second logical instruction.

After a timeout or disconnect, Mainloop reconciles native-session and workspace state before deciding whether to retry. Unknown delivery is not equivalent to failure. Recovery never replays transcript commands automatically.

Check results and review verdicts bind to an exact candidate identity. Integration review covers combined-system concerns such as duplicated foundations, conflicting abstractions, contract drift, and inconsistent security or data handling—not only whether each feature works independently.

Failure handling distinguishes provider availability, transport errors, source defects, integration failures, and missing authority. Recovery and repair use finite limits and stop on repeated no-progress outcomes.

## Workspace platform

The target execution platform provides one isolated writable workspace per concurrent writer, with persistent source and native-session state, bounded resources, scoped development services, preview endpoints, and observable lifecycle state. Reviewers may receive a read-only frozen candidate.

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
