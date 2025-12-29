<script lang="ts">
  import { tasks, tasksList, isTasksOpen, activeTasksCount } from '$lib/stores/tasks';

  let { desktop = false, mobile = false }: { desktop?: boolean; mobile?: boolean } = $props();

  function handleClose() {
    tasks.close();
  }

  function handleClickOutside(e: MouseEvent) {
    // Skip click outside handling for desktop/mobile modes
    if (desktop || mobile) return;

    // Only process if panel is open
    if (!$isTasksOpen) return;

    const target = e.target as HTMLElement;
    if (target.closest('.tasks-panel') || target.closest('[aria-label="Open tasks"]')) {
      return;
    }
    tasks.close();
  }

  function getStatusColor(status: string): string {
    switch (status) {
      case 'pending':
        return 'bg-yellow-100 text-yellow-800';
      case 'planning':
      case 'implementing':
        return 'bg-blue-100 text-blue-800';
      case 'waiting_plan_review':
      case 'waiting_human':
        return 'bg-purple-100 text-purple-800';
      case 'completed':
        return 'bg-green-100 text-green-800';
      case 'failed':
        return 'bg-red-100 text-red-800';
      case 'cancelled':
        return 'bg-neutral-100 text-neutral-800';
      default:
        return 'bg-neutral-100 text-neutral-800';
    }
  }

  function getStatusLabel(status: string): string {
    switch (status) {
      case 'pending':
        return 'Pending';
      case 'planning':
        return 'Planning';
      case 'waiting_plan_review':
        return 'Awaiting Plan Review';
      case 'implementing':
        return 'Implementing';
      case 'waiting_human':
        return 'Awaiting Review';
      case 'completed':
        return 'Completed';
      case 'failed':
        return 'Failed';
      case 'cancelled':
        return 'Cancelled';
      default:
        return status;
    }
  }

  function formatTime(dateStr: string): string {
    const date = new Date(dateStr);
    const now = new Date();
    const diff = now.getTime() - date.getTime();
    const minutes = Math.floor(diff / 60000);
    const hours = Math.floor(minutes / 60);
    const days = Math.floor(hours / 24);

    if (minutes < 1) return 'just now';
    if (minutes < 60) return `${minutes}m ago`;
    if (hours < 24) return `${hours}h ago`;
    return `${days}d ago`;
  }

  function handleCancel(taskId: string) {
    if (confirm('Are you sure you want to cancel this task?')) {
      tasks.cancelTask(taskId);
    }
  }
</script>

<svelte:window on:click={handleClickOutside} />

{#if desktop || mobile || $isTasksOpen}
  <div
    class="tasks-panel flex h-full flex-col bg-white"
    class:fixed={!desktop && !mobile}
    class:right-0={!desktop && !mobile}
    class:top-0={!desktop && !mobile}
    class:z-50={!desktop && !mobile}
    class:w-full={!desktop && !mobile}
    class:max-w-md={!desktop && !mobile}
    class:border-l={!desktop && !mobile}
    class:border-neutral-200={!desktop && !mobile}
    class:shadow-xl={!desktop && !mobile}
  >
    <header class="flex items-center justify-between border-b border-neutral-200 px-4 py-3">
      <div class="flex items-center gap-2">
        <h2 class="text-lg font-semibold text-neutral-900">Tasks</h2>
        {#if $activeTasksCount > 0}
          <span class="rounded-full bg-blue-100 px-2 py-0.5 text-xs font-medium text-blue-800">
            {$activeTasksCount} active
          </span>
        {/if}
      </div>
      {#if !desktop && !mobile}
        <button
          onclick={handleClose}
          class="rounded-lg p-1 transition-colors hover:bg-neutral-100"
          aria-label="Close tasks"
        >
          <svg
            xmlns="http://www.w3.org/2000/svg"
            fill="none"
            viewBox="0 0 24 24"
            stroke-width="1.5"
            stroke="currentColor"
            class="h-6 w-6 text-neutral-600"
          >
            <path stroke-linecap="round" stroke-linejoin="round" d="M6 18 18 6M6 6l12 12" />
          </svg>
        </button>
      {/if}
    </header>

    <div class="flex-1 overflow-y-auto">
      {#if $tasksList.length === 0}
        <div class="flex h-full flex-col items-center justify-center px-4 text-center">
          <svg
            xmlns="http://www.w3.org/2000/svg"
            fill="none"
            viewBox="0 0 24 24"
            stroke-width="1.5"
            stroke="currentColor"
            class="mb-4 h-12 w-12 text-neutral-300"
          >
            <path
              stroke-linecap="round"
              stroke-linejoin="round"
              d="M5.25 14.25h13.5m-13.5 0a3 3 0 0 1-3-3m3 3a3 3 0 1 0 0 6h13.5a3 3 0 1 0 0-6m-16.5-3a3 3 0 0 1 3-3h13.5a3 3 0 0 1 3 3m-19.5 0a4.5 4.5 0 0 1 .9-2.7L5.737 5.1a3.375 3.375 0 0 1 2.7-1.35h7.126c1.062 0 2.062.5 2.7 1.35l2.587 3.45a4.5 4.5 0 0 1 .9 2.7m0 0a3 3 0 0 1-3 3m0 3h.008v.008h-.008v-.008Zm0-6h.008v.008h-.008v-.008Zm-3 6h.008v.008h-.008v-.008Zm0-6h.008v.008h-.008v-.008Z"
            />
          </svg>
          <p class="text-neutral-500">No tasks</p>
          <p class="mt-1 text-sm text-neutral-400">Worker agents will appear here</p>
        </div>
      {:else}
        <div class="divide-y divide-neutral-200">
          {#each $tasksList as task (task.id)}
            <div class="p-4 hover:bg-neutral-50">
              <div class="flex items-start justify-between gap-2">
                <div class="min-w-0 flex-1">
                  <p class="truncate text-sm font-medium text-neutral-900">
                    {task.description.length > 60
                      ? task.description.slice(0, 60) + '...'
                      : task.description}
                  </p>
                  <div class="mt-1 flex flex-wrap items-center gap-2 text-xs text-neutral-500">
                    <span class={`rounded-full px-2 py-0.5 ${getStatusColor(task.status)}`}>
                      {getStatusLabel(task.status)}
                    </span>
                    <span>{formatTime(task.created_at)}</span>
                    {#if task.repo_url}
                      <span class="truncate max-w-32" title={task.repo_url}>
                        {task.repo_url.replace('https://github.com/', '')}
                      </span>
                    {/if}
                  </div>
                  {#if task.pr_url}
                    <a
                      href={task.pr_url}
                      target="_blank"
                      rel="noopener noreferrer"
                      class="mt-2 inline-flex items-center gap-1 text-xs text-blue-600 hover:underline"
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
                    <p class="mt-2 text-xs text-red-600">{task.error}</p>
                  {/if}
                </div>
                {#if !['completed', 'failed', 'cancelled'].includes(task.status)}
                  <button
                    onclick={() => handleCancel(task.id)}
                    class="rounded p-1 text-neutral-400 transition-colors hover:bg-neutral-100 hover:text-red-600"
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
                      <path stroke-linecap="round" stroke-linejoin="round" d="M6 18 18 6M6 6l12 12" />
                    </svg>
                  </button>
                {/if}
              </div>
            </div>
          {/each}
        </div>
      {/if}
    </div>
  </div>
{/if}
