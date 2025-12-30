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
    <div class="flex items-center gap-2 text-sm text-term-fg-muted">
      <span class="animate-cursor text-term-accent">_</span>
      Loading logs...
    </div>
  {:else if error}
    <p class="text-sm text-term-error">{error}</p>
  {:else if !logs}
    <p class="text-sm text-term-fg-muted">No logs available yet</p>
  {:else}
    <div class="mb-2 flex items-center justify-between">
      <span class="text-xs text-term-fg-muted">
        {source === 'k8s' ? 'Live from pod' : 'No pod running'}
      </span>
      {#if source === 'k8s'}
        <span class="flex items-center gap-1 text-xs text-term-accent-alt">
          <span class="h-2 w-2 animate-pulse bg-term-accent-alt"></span>
          LIVE
        </span>
      {/if}
    </div>
    <pre
      bind:this={logsContainer}
      class="max-h-64 overflow-auto border border-term-border bg-term-bg p-3 text-xs whitespace-pre-wrap text-term-fg"
    >{logs}</pre>
  {/if}
</div>
