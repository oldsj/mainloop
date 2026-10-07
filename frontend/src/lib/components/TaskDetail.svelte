<script lang="ts">
  import { goto } from '$app/navigation';
  import type { TaskView } from '$lib/api';
  import { mobileTab } from '$lib/stores/mobileTab';
  import {
    ancestors,
    attemptHistory,
    childrenOf,
    currentAttempt,
    openOperations,
    providerLabel,
    publicationFacts,
    reasonLabel,
    reportLabel,
    statusLabel,
    statusTone,
    toneClass,
    unverifiedSummary,
    workspaceFacts,
    type Fact
  } from '$lib/taskState';
  import TaskActions from './TaskActions.svelte';

  let { view, views }: { view: TaskView; views: TaskView[] } = $props();

  // Observation age only changes with the clock, so tick it while the page is open.
  let now = $state(Date.now());
  $effect(() => {
    const timer = setInterval(() => (now = Date.now()), 30000);
    return () => clearInterval(timer);
  });

  const task = $derived(view.task);
  const attempt = $derived(currentAttempt(view));
  const where = $derived(workspaceFacts(view));
  const facts = $derived(publicationFacts(view.projection, now));
  const history = $derived(attemptHistory(view));
  const operations = $derived(openOperations(view));
  const summary = $derived(unverifiedSummary(view));
  const parents = $derived(ancestors(views, task.id));
  const children = $derived(childrenOf(views, task.id));
  const approvals = $derived(view.projection.pending_approval_ids ?? []);

  function openInbox() {
    mobileTab.set('tasks');
    void goto('/');
  }

  function factClass(fact: Fact): string {
    return fact.known ? toneClass(fact.tone) : 'text-term-fg-muted border-term-border border-dashed';
  }
</script>

<article class="flex flex-col gap-6" data-testid="task-detail">
  <header class="flex flex-col gap-2">
    {#if parents.length > 0}
      <nav aria-label="Parent tasks" class="text-term-fg-muted text-xs">
        {#each parents as parent (parent.task.id)}
          <a href="/tasks/{parent.task.id}" class="hover:underline">{parent.task.title}</a>
          <span aria-hidden="true"> / </span>
        {/each}
      </nav>
    {/if}
    <h1 class="text-term-fg break-words text-lg">{task.title}</h1>
    <div class="flex flex-wrap items-center gap-2 text-xs">
      <span
        class="border px-2 py-0.5 {toneClass(statusTone(task.status))}"
        data-testid="task-status"
      >
        {statusLabel(task.status)}
      </span>
      {#if reasonLabel(task.reason)}
        <span class="text-term-fg-muted">{reasonLabel(task.reason)}</span>
      {/if}
      {#if task.mode === 'coordination'}
        <span class="text-term-fg-muted border-term-border border px-2 py-0.5">Supervisor</span>
      {/if}
      <span class="text-term-fg-muted">version {task.version}</span>
    </div>
    {#if task.brief}
      <p class="text-term-fg-muted text-sm break-words whitespace-pre-wrap">{task.brief}</p>
    {/if}
    {#if approvals.length === 0 && task.status === 'waiting' && task.reason === 'approval'}
      <p class="text-term-yellow text-sm">
        Waiting on owner approval, but the server lists no pending request. Refresh to check.
      </p>
    {/if}
  </header>

  {#if approvals.length > 0}
    <section
      class="border-term-yellow/50 border px-3 py-2"
      aria-labelledby="pending-heading"
      data-testid="task-pending-approvals"
    >
      <h2 id="pending-heading" class="text-term-yellow text-sm">
        {approvals.length} pending owner action{approvals.length === 1 ? '' : 's'}
      </h2>
      <p class="text-term-fg-muted mt-1 text-xs">Answer in the Inbox; the task resumes from there.</p>
      <button
        type="button"
        onclick={openInbox}
        class="border-term-border text-term-fg hover:border-term-accent mt-2 min-h-11 border px-4 py-2 text-sm"
      >
        Open Inbox
      </button>
    </section>
  {/if}

  <section aria-labelledby="provider-heading">
    <h2 id="provider-heading" class="text-term-fg mb-2 text-sm">[PROVIDER]</h2>
    <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
      <dt class="text-term-fg-muted">Provider</dt>
      <dd class="text-term-fg" data-testid="task-provider">{providerLabel(view)}</dd>
      <dt class="text-term-fg-muted">Profile</dt>
      <dd class="text-term-fg break-all">{attempt?.profile_id ?? task.assigned_profile_id}</dd>
      <dt class="text-term-fg-muted">Native session</dt>
      <dd class="text-term-fg break-all">
        {#if where.sessionId}
          <a href="/sessions/{where.sessionId}" class="text-term-info hover:underline"
            >{where.sessionId}</a
          >
        {:else}
          <span class="text-term-fg-muted">none yet</span>
        {/if}
      </dd>
      <dt class="text-term-fg-muted">Activity</dt>
      <dd class="text-term-fg">{view.projection.agent_activity ?? 'unknown'}</dd>
    </dl>
  </section>

  <section aria-labelledby="workspace-heading">
    <h2 id="workspace-heading" class="text-term-fg mb-2 text-sm">[WORKSPACE]</h2>
    <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
      <dt class="text-term-fg-muted">Workspace</dt>
      <dd class="text-term-fg break-all">
        {#if where.workspaceId}
          <a href="/workspaces/{where.workspaceId}" class="text-term-info hover:underline"
            >{where.workspaceId}</a
          >
        {:else}
          <span class="text-term-fg-muted">none yet</span>
        {/if}
      </dd>
      <dt class="text-term-fg-muted">Environment</dt>
      <dd class="text-term-fg break-all">
        {where.environmentVersionId ?? 'unknown'}
      </dd>
      <dt class="text-term-fg-muted">Branch</dt>
      <dd class="text-term-fg break-all">{where.branch ?? 'unknown'}</dd>
      <dt class="text-term-fg-muted">Health</dt>
      <dd class="text-term-fg">{view.projection.workspace_health ?? 'unknown'}</dd>
    </dl>
  </section>

  <section aria-labelledby="publication-heading" data-testid="task-publication">
    <h2 id="publication-heading" class="text-term-fg mb-2 text-sm">[PR · CI · MERGE]</h2>
    <ul class="grid gap-2 text-sm sm:grid-cols-3">
      <li class="border px-3 py-2 {factClass(facts.pr)}">
        <span class="text-term-fg-muted block text-xs">Pull request</span>
        {facts.pr.label}
        {#if facts.prUrl}
          <a
            href={facts.prUrl}
            target="_blank"
            rel="noopener noreferrer"
            class="text-term-info mt-1 block text-xs hover:underline"
          >
            Open on GitHub
          </a>
        {/if}
      </li>
      <li class="border px-3 py-2 {factClass(facts.ci)}">
        <span class="text-term-fg-muted block text-xs">CI</span>
        {facts.ci.label}
        {#if facts.prHeadSha}
          <span class="text-term-fg-muted block text-xs break-all">head {facts.prHeadSha.slice(0, 12)}</span>
        {/if}
      </li>
      <li class="border px-3 py-2 {factClass(facts.merge)}">
        <span class="text-term-fg-muted block text-xs">Merge</span>
        {facts.merge.label}
      </li>
    </ul>
    <p class="text-term-fg-muted mt-2 text-xs">
      Publication: {facts.publication.label}.
      <span class={facts.observation.stale ? 'text-term-yellow' : ''} data-testid="task-observed">
        {facts.observation.text}{facts.observation.stale && facts.observation.known
          ? ' (stale)'
          : ''}.
      </span>
      Each is observed separately; none implies the others.
    </p>
  </section>

  {#if operations.length > 0}
    <section aria-labelledby="operations-heading" data-testid="task-operations">
      <h2 id="operations-heading" class="text-term-fg mb-2 text-sm">[IN PROGRESS]</h2>
      <ul class="flex flex-col gap-3">
        {#each operations as progress (progress.operation.id)}
          <li class="border-term-border border px-3 py-2">
            <p class="text-term-fg text-sm">{progress.summary}</p>
            <ol class="mt-2 flex flex-col gap-0.5 text-xs">
              {#each progress.steps as step (step.state)}
                <li
                  class={step.status === 'current'
                    ? 'text-term-cyan'
                    : step.status === 'done'
                      ? 'text-term-fg'
                      : 'text-term-fg-muted'}
                  aria-current={step.status === 'current' ? 'step' : undefined}
                >
                  <span aria-hidden="true"
                    >{step.status === 'done' ? '✓' : step.status === 'current' ? '›' : '·'}</span
                  >
                  {step.label}<span class="sr-only"> ({step.status})</span>
                </li>
              {/each}
            </ol>
            {#if progress.held}
              <p class="text-term-yellow mt-2 text-xs">
                Held at the last confirmed step. Nothing further happens until the server resolves
                it.
              </p>
            {/if}
          </li>
        {/each}
      </ul>
    </section>
  {/if}

  <TaskActions {view} />

  <section aria-labelledby="attempts-heading" data-testid="task-attempts">
    <h2 id="attempts-heading" class="text-term-fg mb-2 text-sm">[ATTEMPTS]</h2>
    {#if history.length === 0}
      <p class="text-term-fg-muted text-sm">No attempts yet.</p>
    {:else}
      <ol class="flex flex-col gap-2">
        {#each history as row (row.attempt.id)}
          <li class="border-term-border border px-3 py-2 text-sm {row.superseded ? 'opacity-70' : ''}">
            <span class="text-term-fg">
              Attempt {row.attempt.number} · {row.attempt.native_provider}
            </span>
            <span class="text-term-fg-muted text-xs">
              {row.attempt.state}{row.current ? ' · current' : ''}{row.superseded
                ? ' · superseded, history only'
                : ''}
            </span>
            {#if row.predecessorNumber != null}
              <span class="text-term-fg-muted block text-xs">
                continued from attempt {row.predecessorNumber}
              </span>
            {/if}
            {#if row.attempt.checkpoint_ref}
              <span class="text-term-fg-muted block text-xs break-all">
                checkpoint {row.attempt.checkpoint_ref}
              </span>
            {/if}
            {#if row.attempt.session_id}
              <a
                href="/sessions/{row.attempt.session_id}"
                class="text-term-info block text-xs break-all hover:underline"
              >
                session {row.attempt.session_id}
              </a>
            {/if}
          </li>
        {/each}
      </ol>
    {/if}
  </section>

  {#if summary}
    <section aria-labelledby="summary-heading" data-testid="task-unverified-summary">
      <h2 id="summary-heading" class="text-term-fg mb-2 text-sm">[{summary.label.toUpperCase()}]</h2>
      <p class="text-term-fg-muted border-term-border border border-dashed px-3 py-2 text-sm break-words whitespace-pre-wrap">
        {summary.text}
      </p>
    </section>
  {/if}

  {#if view.reports.length > 0}
    <section aria-labelledby="reports-heading">
      <h2 id="reports-heading" class="text-term-fg mb-2 text-sm">[REPORTS]</h2>
      <ul class="flex flex-col gap-2">
        {#each view.reports as report (report.request_id)}
          <li class="border-term-border border border-dashed px-3 py-2 text-sm">
            <span class="text-term-fg-muted block text-xs">{reportLabel(report)}</span>
            <span class="text-term-fg break-words">{report.summary}</span>
          </li>
        {/each}
      </ul>
    </section>
  {/if}

  {#if children.length > 0}
    <section aria-labelledby="children-heading">
      <h2 id="children-heading" class="text-term-fg mb-2 text-sm">[CHILD TASKS]</h2>
      <ul class="flex flex-col gap-1 text-sm">
        {#each children as child (child.task.id)}
          <li>
            <a href="/tasks/{child.task.id}" class="text-term-info hover:underline"
              >{child.task.title}</a
            >
            <span class="text-term-fg-muted text-xs">{statusLabel(child.task.status)}</span>
          </li>
        {/each}
      </ul>
    </section>
  {/if}
</article>
