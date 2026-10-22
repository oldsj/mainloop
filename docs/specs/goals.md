# Goals

Status: **Proposed.** Nothing on this page is implemented. It extends the existing task model
(see [tasks](tasks.md), [delegation](delegation.md)) rather than adding a parallel one, and builds
on [ROADMAP.md](../../ROADMAP.md)'s "Goals with guardrails" and "Tasks, goals and agent context"
directions and issue #263. [Loops](loops.md) (issue #314) is the scheduling primitive a goal
supervisor uses to watch and repair without staying always-on; this page defines the goal object
loops attach to.

## Why (Proposed)

Today a goal lives in an outside supervisor session that drives Mainloop tasks through the API and
tracks progress in its own notes. Mainloop cannot see the goal, its progress, or what it is waiting
on. A goal becomes a first-class Mainloop object so the owner can see it, steer it, and trust its
"done" the same way they already trust a merged pull request.

## The goal object and its lifecycle (Proposed)

A goal is not a new kind of task. It is a **coordination-mode supervisor task** — the existing root
of a task tree, depth 1, `parent_task_id` null, `root_task_id` equal to its own id — with one
additional row, keyed 1:1 on the task id, carrying the fields a plain task does not have: wording,
done-when, allowed scope, and limits. This mirrors how a code task's `checkout` already extends the
shared task row without a second task model (see [tasks](tasks.md), "Identity and state"). A goal's
status, reason, attempts, reports, operations and events are the task's own; nothing about
admission, capacity, writer claims, liveness or settlement changes for it.

Fields recorded at creation:

- **Wording.** The owner's stated goal, verbatim, the same size bound and UTF-8 rules as a task
  brief (16,384 bytes). It is not re-derived or summarized by Mainloop.
- **Done-when.** A small list of evidence conditions (see "Done-when evidence" below) that must all
  hold before the goal can close. Free text alone is not a condition; each entry is one of the
  typed kinds this page defines.
- **Allowed projects and paths.** `allowed_projects`, the project IDs the goal's children may be
  created against — enforced the same way a code task already requires an owner-owned project, so a
  child delegation outside this list is refused before admission, the same shape as today's
  `child_capacity` / `global_capacity` refusals. `allowed_paths`, optional path globs per project,
  describing where the goal expects to touch. Mainloop has no general per-task, per-path write
  restriction today — only the project-level protected-path policy in
  [merge-policy](merge-policy.md) — so `allowed_paths` is advisory, carried in every child's brief,
  until a scoped workspace write boundary exists (see "Open questions").
- **Reserved owner decisions.** A list of decision categories the supervisor must not decide on its
  own and must instead raise as an owner decision. Merging a protected path, or any PR under a
  project with `approval` merge policy, is already mechanically reserved by
  [merge-policy](merge-policy.md) regardless of what a goal names here. Any other category (for
  example, "spend above $X", "touch another team's service") is carried as brief text the supervisor
  is asked to honor; Mainloop cannot yet detect a free-text reserved decision mechanically (see
  "Open questions").
- **Limits.** A quota budget, a wall-clock budget, a maximum number of concurrent children, and a
  maximum number of repair attempts per child, all set at creation:
  - **Quota budget.** An estimated planning budget built from the goal task's own usage rollup (the
    "Own" vs. "Descendants" split in [orchestration-health](orchestration-health.md)'s aggregation
    rules), not a provider-enforced quota — the same honesty [orchestration-health](orchestration-health.md)
    already states for an owner-set token budget. Cache attribution, child attribution and
    cumulative/incremental accounting carry the same gaps described there; a goal's "quota used" is
    a best-effort estimate, labelled as such, not a hard stop proven correct to the token.
  - **Wall-clock budget.** Measured from the goal task's creation, independent of any one attempt's
    own runtime bound (`TASK_MAX_RUNTIME`, which already caps a single attempt — see
    [tasks](tasks.md), "Liveness bounds"). A goal's wall-clock budget can span many supervisor
    attempts over its lifetime.
  - **Max concurrent children.** A per-goal override of the existing per-parent capacity check
    (`TASK_MAX_CHILDREN_PER_PARENT`, default 3): the same admission-time refusal, with the goal's
    own limit in place of the global default.
  - **Max repair attempts per child.** A repair is a new child delegated with an explicit
    `repair_of` link to the child it replaces — the same evidence-link shape
    [orchestration-health](orchestration-health.md)'s "Model fit" section already anticipates for
    report evidence. Once a child's repair count reaches the limit, a further repair of it is
    refused the same way a capacity limit is refused.

None of these fields change after creation in this proposal; see "Open questions" for whether a
later raise should be possible.

## How a goal relates to tasks (Proposed)

A goal's children are ordinary tasks, created and managed exactly as a supervisor manages its direct
children today (see [delegation](delegation.md)): the goal supervisor delegates, reads
`task_get`/`task_list`, cancels, and reports, using the same MCP tools and the same depth-2 cap. A
goal does not get a deeper tree, a different capacity bucket, or a separate event stream — its
children's `task:updated` events, reports and attention items are the same rows any other
supervisor's children produce. What is new is the one-row extension above, and the progress and
evidence rollups below, which read that same tree.

A goal can own loops (see [loops](loops.md)) the same way a task can, as its own scheduling
primitive for watching children and firing repairs without the supervisor staying always-on.

## Progress at a glance (Proposed)

A goal's progress is read, not stored: it is a live rollup over the goal task's own children (and,
for the report line, the whole tree), using exactly the counts and definitions
[tasks](tasks.md)'s explorer and [inbox](inbox.md)'s attention projection already define.

- **Child counts by state.** The number of direct children currently `queued`, `running`,
  `waiting`, `blocked`, `completed`, `failed` and `cancelled`, the same status vocabulary every task
  already carries. This needs no new server concept — ROADMAP's "Tasks as the native unit" already
  names "children done, running, blocked, or waiting on the owner" as an explorer rollup to build
  from existing task reads.
- **Latest report line.** The newest `task_reports` row's one-line summary across the whole tree
  rooted at the goal, ordered by creation time — the same unverified-claim report rows
  [delegation](delegation.md) already describes, just read across more than one task.
  "Unverified claim" labelling applies here exactly as it does on a single task's report.
- **Waiting on the owner.** True when either the goal task itself is `blocked` for an owner decision
  (see "Stopping and asking" below), or any descendant counts as **Needs you** under the existing
  rule: a pending approval, a reason that needs a decision (`owner_decision`, `approval`), or
  `waiting` / `publication` with its PR `open` and CI `success` — the exact rule
  [tasks](tasks.md)'s React status (F6) already implements for the explorer's Needs-you group. A
  goal does not get a second definition of "waiting."

## Done-when evidence (Proposed)

A goal's done-when list is a set of typed conditions, each independently checked against stored,
verified facts — never an agent's claim. This follows the rule [tasks](tasks.md) already states for
a single coding task ("A provider report or finished turn cannot prove coding-task completion") and
extends it to the goal as a whole: a `completed` report from the goal supervisor is not evidence of
anything by itself.

- **A merged pull request.** A named child task's projection shows `merge_state: merged` with a
  `merge_proposal_id`, the same verified completion gate [tasks](tasks.md)'s `task-complete` sweep
  already enforces for one task.
- **A passing check.** A named child task's CI projection reads `success` at the current head,
  under the same freshness rule already enforced for task reads: the CI head must equal the PR head,
  and the observation must be no more than five minutes old (see [tasks](tasks.md), "Go status
  (G8b)").
- **A live health check.** A passing [devloop](devloop.md) `dev_sync` (its `health_ok` and, if
  configured, its smoke result) against the project's configured staging target, read from the
  stored `devloop_operations` row rather than taken on trust. Devloop's staging target is a shared
  dev instance, not production, today — a production health signal is a separate, not-yet-built
  observation (see "Open questions" and the first-slice proposal below).
- **A clear attention list.** The goal's own attention snapshot (the existing
  `attention_visible` view [inbox](inbox.md) already maintains) shows no open item for the goal's
  tree at the moment it is read. Because this is a point-in-time read, closing a goal on this
  condition re-reads it immediately before closing, the same freshness discipline the CI condition
  uses, so a card that reopens in between is not missed.

A goal closes only when every condition in its done-when list currently holds under a fresh read.
No condition is satisfied by a report, a chat message, or the supervisor's own judgment.

## Stopping and asking (Proposed)

A goal stops and raises an attention item in three cases, and the mechanism differs by who notices:

- **A limit is hit** (quota, wall clock, max concurrent children exhausted and blocked on an
  admission attempt, or max repair attempts for some child reached). Mainloop's own guardrail check
  notices this, not the agent — there is no claim to distrust here, only a count. The check blocks
  the goal task (`blocked`, a goal-specific reason such as `goal_limit`) and opens an attention item
  directly, the same way the existing liveness and stop-resolver sweeps already open a
  `task_reconciliation` item with no agent report involved (see [inbox](inbox.md), "Reconciliation
  and merge attention"). Because a limit hit has no further automatic recovery to attempt, this item
  is a real decision (`owner_decision` kind), not the `problem` kind those sweeps use today — unlike
  a stalled or lost session, there is nothing left to retry before asking.
- **Repairs stop converging.** The same guardrail check as above, triggered when a child's repair
  count reaches its limit and the newest repair has itself failed or stalled: there is no next
  automatic repair to try, so this raises the same kind of item, with reason `goal_repair_exhausted`.
- **A reserved decision arises.** The supervisor itself recognizes a reserved decision from its
  brief and raises it the existing way every supervisor already does: `report(outcome=blocked,
owner_action=...)`, which [tasks](tasks.md) already turns into the task's `owner_decision` and an
  attention item. Mainloop does not detect a free-text reserved category on its own; it relies on the
  supervisor's own judgment except where [merge-policy](merge-policy.md)'s protected-path and
  approval-policy checks already enforce a reservation mechanically.

In every case the owner sees the same kind of card the rest of Mainloop already uses for a real
decision — nothing about a goal's stop-and-ask needs a new attention surface, only a new reason code
on the existing one.

## Mobile-first UI (Proposed)

**Goal row**, in the existing explorer alongside other tasks: the wording as its title, the
done-when as a short one-line summary ("Done when: PR merged, checks green"), the child-count
badges from "Progress at a glance," the latest report line, and the same Needs-you / Problems
coloring the explorer already uses for "waiting on the owner." No new iconography — a goal looks
like any other supervisor task row, with a done-when line and child counts added.

**Goal page**: wording and done-when at the top; a limits panel showing each limit alongside how
much of it is used so far (for example, "3 of 5 repairs used on one child," "2h 14m of a 6h
wall-clock budget," an estimated quota figure labelled as an estimate); the reserved-decisions list
and allowed projects/paths; the existing child task tree (the same `TaskTree` component
[tasks](tasks.md) already ships, reused rather than rebuilt); the done-when evidence list with each
condition's current observed state (met / not yet / stale); and the task's own reports and actions,
exactly as a plain task page shows them today, with a stop-and-ask item leading the page the same
way an open `owner_decision` already leads any task page (see [tasks](tasks.md), React status F6).

## Proposed API shape in prose

A goal read extends the existing task read; it does not introduce a second identity for the same
tree.

- **`Goal`** fields: the owning task's id (no separate goal id), `wording`, `done_when` (a list of
  typed conditions, each `{kind: merged_pr | passing_check | live_health_check | clear_attention,
detail}`, all required to hold), `allowed_projects` (project IDs), `allowed_paths` (optional
  per-project globs, advisory), `limits` (`quota_budget`, `wall_clock_seconds`,
  `max_concurrent_children`, `max_repairs_per_child`), `reserved_decisions` (free text categories),
  and a read-only `progress` object: child counts by state, `latest_report`, `waiting_on_owner`, and
  an `evidence` array mirroring `done_when` with each condition's current observed state.
- **`POST /goals`**: creates the root supervisor task and its goal row in one transaction, keyed by
  `request_id` the same way `POST /tasks` is today — a replay with identical input returns the same
  goal; changed input conflicts.
- **`GET /goals`**: an owner-scoped list, the goal analogue of `GET /tasks?parent_task_id=`, limited
  to root tasks that carry a goal row.
- **`GET /goals/{id}`**: the task read (status, attempts, operations, reports, projection) plus the
  goal fields and `progress` above.
- Goal actions reuse the existing task action surface — `POST /tasks/{id}/cancel` cancels a goal the
  same way it cancels any supervisor task, draining its children through the existing settlement
  path. No separate goal-cancel route is proposed.
- A goal-limit stop or a reserved decision surfaces through the existing attention and inbox reads;
  no separate goal-attention route is proposed.

The OpenAPI change implementing this shape is a later PR; this page fixes the shape in prose first.

## First-slice proposal

The smallest useful slice to build first does not need the goal object at all: a single [loop](loops.md)
on one existing task, firing on check-run or pull-request state, so "merge this task's PR when
green" works without the owner polling it. Concretely:

1. The owner (or a supervisor) arms a loop on a code task, watching its own CI/PR projection.
2. The loop fires once when CI turns `success` at the current PR head, delivering one bounded
   message to the task's supervisor: "CI is green on `<task link>`'s PR; current state: `<PR/CI
summary>`."
3. The supervisor, already holding merge authority for its own child, calls the existing
   `merge_pull_request` (or, on the approval route, waits for the owner's existing approval card)
   without the owner having to notice CI turned green first.

This depends on, and builds nothing new beyond:

- [loops](loops.md)'s durable firing and idempotency mechanism (not yet built);
- the existing task-projection CI/PR observation sweep, already implemented (see [tasks](tasks.md),
  "Go status (G8b)");
- the existing sessions-ledger delivery and wake path that already carries reports and HITL
  responses to a busy or suspended session (see [tasks](tasks.md) and [inbox](inbox.md));
- merge tools turned on (`MAINLOOP_MERGE_TOOLS_ENABLED`, off by default today — see
  [merge-policy](merge-policy.md)).

It deliberately does not attempt the "confirm prod" half of the loops issue's own example: Mainloop
has no production health observation today, only [devloop](devloop.md)'s dev-only staging target.
That half needs a new observed signal before a loop can fire on it, and is left to a later slice.
No goal object, done-when evidence list, or limit is needed to prove this slice; the goal
supervisor's own "watch and repair" behavior is this same mechanism, pointed at many children
instead of one.

## Open questions

- Is `allowed_paths` ever mechanically enforced, or does it stay brief-level trust until a scoped
  workspace read/write boundary exists?
- Beyond what [merge-policy](merge-policy.md) already reserves mechanically, can a free-text
  reserved decision ever be detected and enforced by Mainloop itself, or does it stay the
  supervisor's own judgment call?
- Given the usage-accounting gaps [orchestration-health](orchestration-health.md) already
  documents (cache, child attribution, cumulative vs. incremental counters), how should a quota
  limit's "used so far" be computed and labelled so it is never mistaken for a hard stop?
- Can a goal's limits be raised after creation, or does changing a limit require a new goal?
- Can a goal be a child of another goal? If not, what refuses it, and how is that refusal shown?
- Does "repair" require a materially different brief from the child it replaces, or does
  re-delegating the same brief count as a repair attempt too?
- When a goal's done-when is satisfied, do its still-running children keep running to their own
  completion, or does closing the goal cancel them?
