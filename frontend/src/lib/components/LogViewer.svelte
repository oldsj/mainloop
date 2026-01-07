<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { api } from '$lib/api';

  let { taskId, taskStatus }: { taskId: string; taskStatus: string } = $props();

  let logs = $state('');
  let isLoading = $state(true);
  let error = $state<string | null>(null);
  let source = $state<string>('');
  let logsContainer: HTMLDivElement;

  // Auto-scroll state - only scroll if user is near bottom
  let isUserNearBottom = $state(true);
  let wasManuallyScrolled = $state(false);

  let pollInterval: ReturnType<typeof setInterval> | null = null;

  interface LogEntry {
    type: 'system' | 'claude' | 'status' | 'info';
    content: string;
    timestamp?: string;
  }

  // Parse logs into structured entries with types
  const parsedLogs = $derived.by<LogEntry[]>(() => {
    if (!logs) return [];

    const lines = logs.split('\n');
    const entries: LogEntry[] = [];

    for (const line of lines) {
      if (!line.trim()) continue;

      // Match [job_runner] prefixed lines - system/setup messages
      if (line.startsWith('[job_runner]')) {
        const content = line.replace('[job_runner] ', '');
        entries.push({
          type: 'system',
          content: content
        });
      }
      // Match [claude] prefixed lines - Claude's output
      else if (line.startsWith('[claude]')) {
        const content = line.replace('[claude] ', '');
        entries.push({
          type: 'claude',
          content: content
        });
      }
      // Mode/Model info lines
      else if (line.includes('Mode:') || line.includes('Model:') || line.includes('Permission mode:')) {
        entries.push({
          type: 'info',
          content: line.trim()
        });
      }
      // Default - just text
      else {
        entries.push({
          type: 'status',
          content: line
        });
      }
    }

    return entries;
  });

  // Status messages for different task states (before pod logs are available)
  const statusMessage = $derived.by(() => {
    switch (taskStatus) {
      case 'pending':
        return 'Setting up workspace...';
      case 'planning':
        return 'Analyzing codebase and creating plan...';
      case 'waiting_plan_review':
        return 'Plan ready - awaiting review in inbox';
      case 'implementing':
        return 'Implementing changes...';
      case 'under_review':
        return 'Waiting for code review feedback...';
      default:
        return null;
    }
  });

  function checkIfNearBottom() {
    if (!logsContainer) return true;
    const threshold = 100; // pixels from bottom
    const position = logsContainer.scrollHeight - logsContainer.scrollTop - logsContainer.clientHeight;
    return position < threshold;
  }

  function handleScroll() {
    isUserNearBottom = checkIfNearBottom();
    if (!isUserNearBottom) {
      wasManuallyScrolled = true;
    }
  }

  function scrollToBottom() {
    if (logsContainer && isUserNearBottom) {
      requestAnimationFrame(() => {
        logsContainer.scrollTop = logsContainer.scrollHeight;
      });
    }
  }

  // Jump to bottom button handler
  function jumpToBottom() {
    if (logsContainer) {
      logsContainer.scrollTop = logsContainer.scrollHeight;
      isUserNearBottom = true;
      wasManuallyScrolled = false;
    }
  }

  async function fetchLogs() {
    try {
      const result = await api.getTaskLogs(taskId, 200);
      const logsChanged = logs !== result.logs;
      logs = result.logs;
      source = result.source;
      error = null;

      // Only auto-scroll if logs changed and user is near bottom
      if (logsChanged && isUserNearBottom) {
        scrollToBottom();
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

    // Initial scroll to bottom
    requestAnimationFrame(() => {
      if (logsContainer) {
        logsContainer.scrollTop = logsContainer.scrollHeight;
      }
    });
  });

  onDestroy(() => {
    if (pollInterval) {
      clearInterval(pollInterval);
    }
  });

  function getEntryStyle(type: LogEntry['type']): string {
    switch (type) {
      case 'system':
        return 'text-term-fg-muted';
      case 'claude':
        return 'text-term-accent-alt';
      case 'info':
        return 'text-term-info';
      case 'status':
      default:
        return 'text-term-fg';
    }
  }

</script>

<div class="flex flex-col gap-3 p-4">
  {#if isLoading}
    <div class="flex items-center gap-2 text-sm text-term-fg-muted">
      <span class="animate-cursor text-term-accent">_</span>
      Loading activity...
    </div>
  {:else if error}
    <p class="text-sm text-term-error">{error}</p>
  {:else}
    <!-- Header with source indicator -->
    <div class="flex items-center justify-between">
      <span class="text-xs text-term-fg-muted">
        {source === 'k8s' ? 'Live from worker' : statusMessage || 'No worker running'}
      </span>
      {#if source === 'k8s'}
        <span class="flex items-center gap-1 text-xs text-term-accent-alt">
          <span class="h-2 w-2 animate-pulse bg-term-accent-alt"></span>
          LIVE
        </span>
      {/if}
    </div>

    <!-- Activity log -->
    <div class="relative">
      <div
        bind:this={logsContainer}
        onscroll={handleScroll}
        class="max-h-64 overflow-auto border border-term-border bg-term-bg p-3"
      >
        {#if parsedLogs.length === 0}
          <!-- No logs yet - show status-based activity -->
          <div class="flex flex-col gap-2">
            {#if taskStatus === 'pending'}
              <div class="flex items-center gap-2 text-sm text-term-fg-muted">
                <svg class="h-3 w-3 animate-spin" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                  <circle cx="12" cy="12" r="10" stroke-opacity="0.25" />
                  <path d="M12 2a10 10 0 0 1 10 10" />
                </svg>
                <span>Creating isolated workspace...</span>
              </div>
              <div class="flex items-center gap-2 text-sm text-term-fg-muted">
                <svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                  <rect x="3" y="11" width="18" height="11" rx="2" ry="2" />
                  <path d="M7 11V7a5 5 0 0 1 10 0v4" />
                </svg>
                <span>Copying credentials...</span>
              </div>
            {:else if taskStatus === 'planning'}
              <div class="flex items-center gap-2 text-sm text-term-info">
                <svg class="h-3 w-3 animate-pulse" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                  <path d="M12 2L2 7l10 5 10-5-10-5z" />
                  <path d="M2 17l10 5 10-5" />
                  <path d="M2 12l10 5 10-5" />
                </svg>
                <span>Claude is exploring the codebase...</span>
              </div>
            {:else if taskStatus === 'implementing'}
              <div class="flex items-center gap-2 text-sm text-term-accent-alt">
                <svg class="h-3 w-3 animate-pulse" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                  <polyline points="16 18 22 12 16 6" />
                  <polyline points="8 6 2 12 8 18" />
                </svg>
                <span>Claude is writing code...</span>
              </div>
            {:else if taskStatus === 'waiting_plan_review'}
              <div class="flex items-center gap-2 text-sm text-term-warning">
                <svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                  <circle cx="12" cy="12" r="10" />
                  <line x1="12" y1="8" x2="12" y2="12" />
                  <line x1="12" y1="16" x2="12.01" y2="16" />
                </svg>
                <span>Waiting for your review in inbox</span>
              </div>
            {:else}
              <p class="text-sm text-term-fg-muted">Waiting for activity...</p>
            {/if}
          </div>
        {:else}
          <!-- Parsed log entries with visual tags -->
          <div class="flex flex-col gap-1">
            {#each parsedLogs as entry, i (i)}
              <div class="flex items-start gap-2 text-xs {getEntryStyle(entry.type)}">
                <!-- Type indicator -->
                {#if entry.type === 'system'}
                  <span class="mt-0.5 shrink-0 rounded bg-term-fg-muted/20 px-1 py-0.5 text-[10px] uppercase text-term-fg-muted">sys</span>
                {:else if entry.type === 'claude'}
                  <span class="mt-0.5 shrink-0 rounded bg-term-accent-alt/20 px-1 py-0.5 text-[10px] uppercase text-term-accent-alt">ai</span>
                {:else if entry.type === 'info'}
                  <span class="mt-0.5 shrink-0 rounded bg-term-info/20 px-1 py-0.5 text-[10px] uppercase text-term-info">cfg</span>
                {:else}
                  <span class="mt-0.5 w-6 shrink-0"></span>
                {/if}
                <!-- Content -->
                <span class="whitespace-pre-wrap break-all">{entry.content}</span>
              </div>
            {/each}
          </div>
        {/if}
      </div>

      <!-- Jump to bottom button - show when user has scrolled up -->
      {#if wasManuallyScrolled && !isUserNearBottom && parsedLogs.length > 0}
        <button
          type="button"
          onclick={jumpToBottom}
          class="absolute bottom-2 right-2 flex items-center gap-1 rounded border border-term-border bg-term-bg px-2 py-1 text-xs text-term-fg-muted shadow transition-colors hover:bg-term-selection hover:text-term-fg"
        >
          <svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <polyline points="6 9 12 15 18 9" />
          </svg>
          Latest
        </button>
      {/if}
    </div>
  {/if}
</div>
