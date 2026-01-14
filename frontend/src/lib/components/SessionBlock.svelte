<script lang="ts">
  import type { Session } from '$lib/api';
  import { navigationContext } from '$lib/stores/navigationContext';

  let {
    session,
    isActive = false,
    onSelect
  }: {
    session: Session;
    isActive?: boolean;
    onSelect?: () => void;
  } = $props();

  // Determine the border/accent color
  const sessionColor = $derived(session.color);

  // Status display
  const statusLabels: Record<string, string> = {
    pending: 'PENDING',
    active: 'ACTIVE',
    waiting_on_user: 'NEEDS INPUT',
    waiting_questions: 'NEEDS INPUT',
    waiting_plan_review: 'REVIEW PLAN',
    ready_to_implement: 'READY',
    planning: 'PLANNING',
    implementing: 'IMPLEMENTING',
    under_review: 'IN REVIEW',
    completed: 'DONE',
    failed: 'FAILED',
    cancelled: 'CANCELLED'
  };

  const isRunning = $derived(
    ['pending', 'active', 'planning', 'implementing'].includes(session.status)
  );

  const needsAttention = $derived(
    ['waiting_on_user', 'waiting_questions', 'waiting_plan_review'].includes(session.status)
  );

  function handleClick() {
    if (onSelect) {
      onSelect();
    } else {
      navigationContext.switchToSession(session.id);
    }
  }

  function handleZoom(e: MouseEvent) {
    e.stopPropagation();
    navigationContext.zoomSession(session.id);
  }
</script>

<div
  class="session-block my-3 ml-4 border-l-4 bg-term-bg-secondary"
  style="border-color: {sessionColor};"
>
  <!-- Header only - thread messages appear as notifications in timeline -->
  <div class="flex w-full items-center justify-between gap-2 px-3 py-2">
    <button
      type="button"
      class="flex min-w-0 flex-1 items-center gap-2 text-left hover:opacity-80"
      onclick={handleClick}
    >
      <!-- Status indicator -->
      {#if isRunning}
        <span class="h-3 w-3 shrink-0 animate-spin rounded-full border border-current border-t-transparent text-term-cyan"></span>
      {/if}

      <!-- Title -->
      <span class="truncate text-sm font-medium text-term-fg">
        {session.title}
      </span>

      <!-- Status badge -->
      <span class="shrink-0 text-xs {needsAttention ? 'text-term-magenta' : 'text-term-fg-muted'}">
        [{statusLabels[session.status] || session.status.toUpperCase()}]
      </span>
    </button>

    <!-- Zoom button -->
    <button
      type="button"
      class="text-xs text-term-fg-muted hover:text-term-accent"
      onclick={handleZoom}
      aria-label="Open session fullscreen"
    >
      <svg xmlns="http://www.w3.org/2000/svg" class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
        <path stroke-linecap="round" stroke-linejoin="round" d="M4 8V4m0 0h4M4 4l5 5m11-1V4m0 0h-4m4 0l-5 5M4 16v4m0 0h4m-4 0l5-5m11 5l-5-5m5 5v-4m0 4h-4" />
      </svg>
    </button>
  </div>
</div>
