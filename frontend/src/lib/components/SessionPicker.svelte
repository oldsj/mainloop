<script lang="ts">
  import type { Session } from '$lib/api';
  import { navigationContext, sessionsByUrgency, getUrgencyScore } from '$lib/stores/navigationContext';

  let {
    onClose,
    onNewSession
  }: {
    onClose?: () => void;
    onNewSession?: () => void;
  } = $props();

  let selectedIndex = $state(0);
  let listElement: HTMLDivElement | null = null;

  // Use urgency-sorted sessions
  const sortedSessions = $derived($sessionsByUrgency);

  // Status styling
  const statusColors: Record<string, string> = {
    waiting_on_user: 'text-term-magenta',
    waiting_questions: 'text-term-magenta',
    waiting_plan_review: 'text-term-magenta',
    active: 'text-term-cyan',
    planning: 'text-term-cyan',
    implementing: 'text-term-cyan',
    pending: 'text-term-yellow',
    completed: 'text-term-green',
    failed: 'text-term-red',
    cancelled: 'text-term-fg-muted'
  };

  const statusLabels: Record<string, string> = {
    waiting_on_user: 'NEEDS INPUT',
    waiting_questions: 'NEEDS INPUT',
    waiting_plan_review: 'REVIEW PLAN',
    active: 'ACTIVE',
    planning: 'PLANNING',
    implementing: 'IMPLEMENTING',
    pending: 'PENDING',
    completed: 'DONE',
    failed: 'FAILED',
    cancelled: 'CANCELLED'
  };

  function selectSession(session: Session) {
    navigationContext.switchToSession(session.id);
    if (onClose) onClose();
  }

  function selectMain() {
    navigationContext.switchToMain();
    if (onClose) onClose();
  }

  function handleNewSession() {
    if (onNewSession) {
      onNewSession();
    }
    if (onClose) onClose();
  }

  function handleKeydown(e: KeyboardEvent) {
    const totalItems = sortedSessions.length + 2; // +1 for main, +1 for new session

    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        selectedIndex = (selectedIndex + 1) % totalItems;
        scrollToSelected();
        break;
      case 'ArrowUp':
        e.preventDefault();
        selectedIndex = (selectedIndex - 1 + totalItems) % totalItems;
        scrollToSelected();
        break;
      case 'Enter':
        e.preventDefault();
        if (selectedIndex === 0) {
          selectMain();
        } else if (selectedIndex === totalItems - 1) {
          handleNewSession();
        } else {
          const session = sortedSessions[selectedIndex - 1];
          if (session) selectSession(session);
        }
        break;
      case 'Escape':
        e.preventDefault();
        if (onClose) onClose();
        break;
    }
  }

  function scrollToSelected() {
    if (listElement) {
      const selected = listElement.querySelector('[data-selected="true"]');
      if (selected) {
        selected.scrollIntoView({ block: 'nearest' });
      }
    }
  }
</script>

<svelte:window onkeydown={handleKeydown} />

<!-- Backdrop -->
<button
  type="button"
  class="fixed inset-0 z-40 bg-black/50"
  onclick={onClose}
  aria-label="Close picker"
></button>

<!-- Picker menu -->
<div
  class="fixed left-1/2 top-1/4 z-50 w-full max-w-md -translate-x-1/2 border border-term-border bg-term-bg shadow-lg"
  role="listbox"
  aria-label="Session picker"
>
  <div class="border-b border-term-border px-3 py-2">
    <span class="text-xs text-term-fg-muted">Quick Switch (↑↓ to navigate, Enter to select, Esc to close)</span>
  </div>

  <div bind:this={listElement} class="max-h-64 overflow-y-auto">
    <!-- Main thread option -->
    <button
      type="button"
      class="flex w-full items-center gap-2 px-3 py-2 text-left {selectedIndex === 0
        ? 'bg-term-accent/20'
        : 'hover:bg-term-bg-secondary'}"
      data-selected={selectedIndex === 0}
      onclick={selectMain}
      role="option"
      aria-selected={selectedIndex === 0}
    >
      <span class="h-2 w-2 rounded-full bg-term-fg"></span>
      <span class="flex-1 text-sm font-medium text-term-fg">Main Thread</span>
      <span class="text-xs text-term-fg-muted">[Shift+Tab]</span>
    </button>

    <!-- Divider -->
    {#if sortedSessions.length > 0}
      <div class="border-t border-term-border"></div>
    {/if}

    <!-- Session list -->
    {#each sortedSessions as session, i (session.id)}
      {@const itemIndex = i + 1}
      {@const isSelected = selectedIndex === itemIndex}
      {@const needsAttention = ['waiting_on_user', 'waiting_questions', 'waiting_plan_review'].includes(session.status)}
      <button
        type="button"
        class="flex w-full items-center gap-2 px-3 py-2 text-left {isSelected
          ? 'bg-term-accent/20'
          : 'hover:bg-term-bg-secondary'} {needsAttention ? 'bg-term-magenta/5' : ''}"
        data-selected={isSelected}
        onclick={() => selectSession(session)}
        role="option"
        aria-selected={isSelected}
      >
        <span
          class="h-2 w-2 shrink-0 rounded-full"
          style="background-color: {session.color};"
        ></span>
        <span class="min-w-0 flex-1 truncate text-sm text-term-fg">
          {session.title}
        </span>
        <span
          class="shrink-0 whitespace-nowrap px-2 py-0.5 text-xs font-medium {needsAttention
            ? 'bg-term-magenta/20 text-term-magenta'
            : session.status === 'completed'
              ? 'bg-term-green/20 text-term-green'
              : session.status === 'failed' || session.status === 'cancelled'
                ? 'bg-term-red/20 text-term-red'
                : ['active', 'planning', 'implementing'].includes(session.status)
                  ? 'bg-term-cyan/20 text-term-cyan'
                  : 'bg-term-fg-muted/20 text-term-fg-muted'}"
          style="border-radius: 9999px;"
        >
          {statusLabels[session.status] || session.status.toUpperCase()}
        </span>
        {#if needsAttention}
          <span class="animate-pulse text-term-magenta">*</span>
        {/if}
      </button>
    {/each}

    <!-- Divider -->
    <div class="border-t border-term-border"></div>

    <!-- New session option -->
    <button
      type="button"
      class="flex w-full items-center gap-2 px-3 py-2 text-left {selectedIndex === sortedSessions.length + 1
        ? 'bg-term-accent/20'
        : 'hover:bg-term-bg-secondary'}"
      data-selected={selectedIndex === sortedSessions.length + 1}
      onclick={handleNewSession}
      role="option"
      aria-selected={selectedIndex === sortedSessions.length + 1}
    >
      <span class="text-term-accent">+</span>
      <span class="text-sm text-term-fg-muted">New session</span>
    </button>
  </div>
</div>
