<script lang="ts">
  import { tasks, tasksList, isTasksOpen, activeTasksCount } from '$lib/stores/tasks';
  import LogViewer from './LogViewer.svelte';

  let { desktop = false, mobile = false }: { desktop?: boolean; mobile?: boolean } = $props();

  let expandedTaskId = $state<string | null>(null);

  function handleClose() {
    tasks.close();
  }

  function handleClickOutside(e: MouseEvent) {
    if (desktop || mobile) return;
    if (!$isTasksOpen) return;

    const target = e.target as HTMLElement;
    if (target.closest('.tasks-panel') || target.closest('[aria-label="Open tasks"]')) {
      return;
    }
    tasks.close();
  }

  function getStatusStyle(status: string): string {
    switch (status) {
      case 'pending':
        return 'border-term-warning text-term-warning';
      case 'planning':
      case 'implementing':
        return 'border-term-info text-term-info';
      case 'waiting_plan_review':
      case 'under_review':
        return 'border-term-accent text-term-accent';
      case 'completed':
        return 'border-term-accent-alt text-term-accent-alt';
      case 'failed':
        return 'border-term-error text-term-error';
      case 'cancelled':
        return 'border-term-fg-muted text-term-fg-muted';
      default:
        return 'border-term-fg-muted text-term-fg-muted';
    }
  }

  function getStatusLabel(status: string): string {
    switch (status) {
      case 'pending':
        return 'PENDING';
      case 'planning':
        return 'PLANNING';
      case 'waiting_plan_review':
        return 'PLAN_REVIEW';
      case 'implementing':
        return 'IMPLEMENTING';
      case 'under_review':
        return 'REVIEW';
      case 'completed':
        return 'DONE';
      case 'failed':
        return 'FAILED';
      case 'cancelled':
        return 'CANCELLED';
      default:
        return status.toUpperCase();
    }
  }

  function formatTime(dateStr: string): string {
    const date = new Date(dateStr);
    const now = new Date();
    const diff = now.getTime() - date.getTime();
    const minutes = Math.floor(diff / 60000);
    const hours = Math.floor(minutes / 60);
    const days = Math.floor(hours / 24);

    if (minutes < 1) return 'now';
    if (minutes < 60) return `${minutes}m`;
    if (hours < 24) return `${hours}h`;
    return `${days}d`;
  }

  function handleCancel(taskId: string) {
    if (confirm('Cancel this task?')) {
      tasks.cancelTask(taskId);
    }
  }

  function handleRetry(taskId: string) {
    tasks.retryTask(taskId);
  }

  function toggleExpand(taskId: string, status: string) {
    const terminalStatuses = ['completed', 'failed', 'cancelled'];
    if (terminalStatuses.includes(status)) return;

    expandedTaskId = expandedTaskId === taskId ? null : taskId;
  }

  function isExpandable(status: string): boolean {
    return !['completed', 'failed', 'cancelled'].includes(status);
  }
</script>

<svelte:window on:click={handleClickOutside} />

{#if desktop || mobile || $isTasksOpen}
  <div
    class="tasks-panel flex h-full flex-col bg-term-bg"
    class:fixed={!desktop && !mobile}
    class:right-0={!desktop && !mobile}
    class:top-0={!desktop && !mobile}
    class:z-50={!desktop && !mobile}
    class:w-full={!desktop && !mobile}
    class:max-w-md={!desktop && !mobile}
    class:border-l={!desktop && !mobile}
    class:border-term-border={!desktop && !mobile}
    class:shadow-xl={!desktop && !mobile}
  >
    <header class="flex items-center justify-between border-b border-term-border px-4 py-3">
      <div class="flex items-center gap-2">
        <h2 class="text-term-fg">[TASKS]</h2>
        {#if $activeTasksCount > 0}
          <span class="border border-term-info px-2 py-0.5 text-xs text-term-info">
            {$activeTasksCount} active
          </span>
        {/if}
      </div>
      {#if !desktop && !mobile}
        <button
          onclick={handleClose}
          class="p-1 text-term-fg-muted transition-colors hover:text-term-fg"
          aria-label="Close tasks"
        >
          <svg
            xmlns="http://www.w3.org/2000/svg"
            fill="none"
            viewBox="0 0 24 24"
            stroke-width="1.5"
            stroke="currentColor"
            class="h-6 w-6"
          >
            <path stroke-linecap="square" stroke-linejoin="miter" d="M6 18 18 6M6 6l12 12" />
          </svg>
        </button>
      {/if}
    </header>

    <div class="flex-1 overflow-y-auto">
      {#if $tasksList.length === 0}
        <div class="flex h-full flex-col items-center justify-center px-4 text-center">
          <p class="text-term-fg-muted">$ ls tasks/</p>
          <p class="mt-2 text-term-fg-muted">No tasks found</p>
          <p class="mt-1 text-sm text-term-fg-muted">Workers will appear here</p>
        </div>
      {:else}
        <div class="divide-y divide-term-border">
          {#each $tasksList as task (task.id)}
            <div class="border-b border-term-border last:border-b-0">
              <!-- svelte-ignore a11y_no_static_element_interactions a11y_no_noninteractive_tabindex -->
              <div
                onclick={() => toggleExpand(task.id, task.status)}
                onkeydown={(e) => e.key === 'Enter' && toggleExpand(task.id, task.status)}
                role={isExpandable(task.status) ? 'button' : undefined}
                tabindex={isExpandable(task.status) ? 0 : undefined}
                class="w-full p-4 text-left transition-colors {isExpandable(task.status)
                  ? 'cursor-pointer hover:bg-term-selection'
                  : ''}"
              >
                <div class="flex items-start justify-between gap-2">
                  <div class="min-w-0 flex-1">
                    <p class="truncate text-sm text-term-fg">
                      {task.description.length > 60
                        ? task.description.slice(0, 60) + '...'
                        : task.description}
                    </p>
                    <div class="mt-1 flex flex-wrap items-center gap-2 text-xs">
                      <span class={`border px-2 py-0.5 ${getStatusStyle(task.status)}`}>
                        {getStatusLabel(task.status)}
                      </span>
                      <span class="text-term-fg-muted">{formatTime(task.created_at)}</span>
                      {#if task.repo_url}
                        <span class="max-w-32 truncate text-term-fg-muted" title={task.repo_url}>
                          {task.repo_url.replace('https://github.com/', '')}
                        </span>
                      {/if}
                    </div>
                    {#if task.issue_url}
                      <a
                        href={task.issue_url}
                        target="_blank"
                        rel="noopener noreferrer"
                        onclick={(e) => e.stopPropagation()}
                        class="mt-2 inline-flex items-center gap-1 text-xs text-term-accent hover:underline"
                      >
                        <svg class="h-3 w-3" viewBox="0 0 16 16" fill="currentColor">
                          <path d="M8 9.5a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3Z" />
                          <path
                            d="M8 0a8 8 0 1 1 0 16A8 8 0 0 1 8 0ZM1.5 8a6.5 6.5 0 1 0 13 0 6.5 6.5 0 0 0-13 0Z"
                          />
                        </svg>
                        Issue #{task.issue_number}
                      </a>
                    {/if}
                    {#if task.pr_url}
                      <a
                        href={task.pr_url}
                        target="_blank"
                        rel="noopener noreferrer"
                        onclick={(e) => e.stopPropagation()}
                        class="mt-2 inline-flex items-center gap-1 text-xs text-term-info hover:underline"
                      >
                        <svg class="h-3 w-3" viewBox="0 0 16 16" fill="currentColor">
                          <path
                            d="M7.177 3.073L9.573.677A.25.25 0 0110 .854v4.792a.25.25 0 01-.427.177L7.177 3.427a.25.25 0 010-.354zM3.75 2.5a.75.75 0 100 1.5.75.75 0 000-1.5zm-2.25.75a2.25 2.25 0 113 2.122v5.256a2.251 2.251 0 11-1.5 0V5.372A2.25 2.25 0 011.5 3.25zM11 2.5h-1V4h1a1 1 0 011 1v5.628a2.251 2.251 0 101.5 0V5A2.5 2.5 0 0011 2.5zm1 10.25a.75.75 0 111.5 0 .75.75 0 01-1.5 0zM3.75 12a.75.75 0 100 1.5.75.75 0 000-1.5z"
                          />
                        </svg>
                        PR #{task.pr_number}
                      </a>
                    {/if}
                    {#if task.error}
                      <p class="mt-2 text-xs text-term-error">{task.error}</p>
                    {/if}
                  </div>

                  <div class="flex items-center gap-1">
                    {#if task.status === 'failed'}
                      <button
                        onclick={(e) => {
                          e.stopPropagation();
                          handleRetry(task.id);
                        }}
                        class="p-1 text-term-fg-muted transition-colors hover:text-term-info"
                        aria-label="Retry task"
                        title="Retry task"
                      >
                        <svg
                          xmlns="http://www.w3.org/2000/svg"
                          fill="none"
                          viewBox="0 0 24 24"
                          stroke-width="1.5"
                          stroke="currentColor"
                          class="h-4 w-4"
                        >
                          <path
                            stroke-linecap="square"
                            stroke-linejoin="miter"
                            d="M16.023 9.348h4.992v-.001M2.985 19.644v-4.992m0 0h4.992m-4.993 0 3.181 3.183a8.25 8.25 0 0 0 13.803-3.7M4.031 9.865a8.25 8.25 0 0 1 13.803-3.7l3.181 3.182m0-4.991v4.99"
                          />
                        </svg>
                      </button>
                    {/if}
                    {#if isExpandable(task.status)}
                      <button
                        onclick={(e) => {
                          e.stopPropagation();
                          handleCancel(task.id);
                        }}
                        class="p-1 text-term-fg-muted transition-colors hover:text-term-error"
                        aria-label="Cancel task"
                        title="Cancel task"
                      >
                        <svg
                          xmlns="http://www.w3.org/2000/svg"
                          fill="none"
                          viewBox="0 0 24 24"
                          stroke-width="1.5"
                          stroke="currentColor"
                          class="h-4 w-4"
                        >
                          <path
                            stroke-linecap="square"
                            stroke-linejoin="miter"
                            d="M6 18 18 6M6 6l12 12"
                          />
                        </svg>
                      </button>

                      <svg
                        xmlns="http://www.w3.org/2000/svg"
                        fill="none"
                        viewBox="0 0 24 24"
                        stroke-width="1.5"
                        stroke="currentColor"
                        class="h-4 w-4 text-term-fg-muted transition-transform {expandedTaskId ===
                        task.id
                          ? 'rotate-180'
                          : ''}"
                      >
                        <path
                          stroke-linecap="square"
                          stroke-linejoin="miter"
                          d="m19.5 8.25-7.5 7.5-7.5-7.5"
                        />
                      </svg>
                    {/if}
                  </div>
                </div>
              </div>

              {#if expandedTaskId === task.id}
                <div class="border-t border-term-border bg-term-bg-secondary">
                  <LogViewer taskId={task.id} taskStatus={task.status} />
                </div>
              {/if}
            </div>
          {/each}
        </div>
      {/if}
    </div>
  </div>
{/if}
