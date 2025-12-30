<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { api } from '$lib/api';

  let { taskId, taskStatus }: { taskId: string; taskStatus: string } = $props();

  let logs = $state('');
  let isLoading = $state(true);
  let error = $state<string | null>(null);
  let source = $state<string>('');
  let logsContainer: HTMLPreElement;

  let pollInterval: ReturnType<typeof setInterval> | null = null;

  async function fetchLogs() {
    try {
      const result = await api.getTaskLogs(taskId, 200);
      logs = result.logs;
      source = result.source;
      error = null;

      // Auto-scroll to bottom on new logs
      if (logsContainer) {
        requestAnimationFrame(() => {
          logsContainer.scrollTop = logsContainer.scrollHeight;
        });
      }
    } catch (e) {
      error = 'Failed to load logs';
    } finally {
      isLoading = false;
    }
  }

  onMount(() => {
    fetchLogs();

    // Only poll for active tasks
    const activeStatuses = ['planning', 'implementing', 'pending'];
    if (activeStatuses.includes(taskStatus)) {
      pollInterval = setInterval(fetchLogs, 3000);
    }
  });

  onDestroy(() => {
    if (pollInterval) {
      clearInterval(pollInterval);
    }
  });
</script>

<div class="p-4">
  {#if isLoading}
    <div class="flex items-center gap-2 text-sm text-neutral-500">
      <svg class="h-4 w-4 animate-spin" viewBox="0 0 24 24" fill="none">
        <circle
          class="opacity-25"
          cx="12"
          cy="12"
          r="10"
          stroke="currentColor"
          stroke-width="4"
        />
        <path
          class="opacity-75"
          fill="currentColor"
          d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"
        />
      </svg>
      Loading logs...
    </div>
  {:else if error}
    <p class="text-sm text-red-600">{error}</p>
  {:else if !logs}
    <p class="text-sm text-neutral-500">No logs available yet</p>
  {:else}
    <div class="mb-2 flex items-center justify-between">
      <span class="text-xs text-neutral-400">
        {source === 'k8s' ? 'Live from pod' : 'No pod running'}
      </span>
      {#if source === 'k8s'}
        <span class="flex items-center gap-1 text-xs text-green-600">
          <span class="h-2 w-2 animate-pulse rounded-full bg-green-500"></span>
          Live
        </span>
      {/if}
    </div>
    <pre
      bind:this={logsContainer}
      class="max-h-64 overflow-auto rounded-lg bg-neutral-900 p-3 font-mono text-xs whitespace-pre-wrap text-neutral-100"
    >{logs}</pre>
  {/if}
</div>
