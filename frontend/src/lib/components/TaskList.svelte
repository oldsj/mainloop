<script lang="ts">
  import type { TaskView } from '$lib/api';
  import {
    buildTree,
    openOperations,
    providerLabel,
    publicationFacts,
    reasonLabel,
    statusLabel,
    statusTone,
    toneClass
  } from '$lib/taskState';

  let {
    views,
    loading = false,
    error = null,
    emptyText = 'No tasks yet.'
  }: {
    views: TaskView[];
    loading?: boolean;
    error?: string | null;
    emptyText?: string;
  } = $props();

  let now = $state(Date.now());
  $effect(() => {
    const timer = setInterval(() => (now = Date.now()), 30000);
    return () => clearInterval(timer);
  });

  const rows = $derived(buildTree(views));
</script>

<div data-testid="task-list">
  {#if error}
    <p class="border-term-red/50 text-term-red border px-3 py-2 text-sm" role="alert">
      Could not load tasks: {error}
      {#if views.length > 0}<span class="text-term-fg-muted"> Showing the last known state.</span
        >{/if}
    </p>
  {/if}

  {#if loading && views.length === 0}
    <p class="text-term-fg-muted py-2 text-sm" role="status">Loading tasks…</p>
  {:else if views.length === 0 && !error}
    <p class="text-term-fg-muted py-2 text-sm">{emptyText}</p>
  {:else}
    <ul class="divide-term-border divide-y">
      {#each rows as row (row.view.task.id)}
        {@const task = row.view.task}
        {@const ci = publicationFacts(row.view.projection, now).ci}
        {@const open = openOperations(row.view)}
        <li style:padding-left="{Math.min(row.depth, 3) * 1}rem">
          <a
            href="/tasks/{task.id}"
            class="hover:bg-term-bg-secondary block py-2 pr-2 {row.depth > 0
              ? 'border-term-border ml-2 border-l pl-3'
              : ''}"
          >
            <span class="flex flex-wrap items-center gap-x-2 gap-y-1">
              <span class="text-term-fg min-w-0 break-words">{task.title}</span>
              <span
                class="border px-2 py-0.5 text-xs {toneClass(statusTone(task.status))}"
                data-testid="task-status"
              >
                {statusLabel(task.status)}
              </span>
              {#if task.mode === 'coordination'}
                <span class="text-term-fg-muted border-term-border border px-2 py-0.5 text-xs">
                  Supervisor
                </span>
              {/if}
            </span>
            <span class="text-term-fg-muted mt-1 flex flex-wrap gap-x-3 gap-y-0.5 text-xs">
              <span>{providerLabel(row.view)}</span>
              {#if reasonLabel(task.reason)}<span>{reasonLabel(task.reason)}</span>{/if}
              {#if open.length > 0}<span>{open[0].summary}</span>{/if}
              <span>CI {ci.known ? ci.label.toLowerCase() : 'unknown'}</span>
              {#if row.detached}<span>child of a task not shown here</span>{/if}
            </span>
          </a>
        </li>
      {/each}
    </ul>
  {/if}
</div>
