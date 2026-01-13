<script lang="ts">
  import { api, type Session, type Message } from '$lib/api';
  import { navigationContext } from '$lib/stores/navigationContext';
  import { marked } from 'marked';

  let {
    session,
    isActive = false,
    onSelect
  }: {
    session: Session;
    isActive?: boolean;
    onSelect?: () => void;
  } = $props();

  // Session messages (fetched when expanded)
  let messages = $state<Message[]>([]);
  let messagesLoading = $state(false);
  let lastFetchedSessionId = $state<string | null>(null);

  // Expanded by default - collapse for terminal states OR when this session is focused
  // (when focused, the inline thread section at bottom shows the conversation)
  const isExpanded = $derived(
    !['completed', 'failed', 'cancelled'].includes(session.status) && !isActive
  );

  // Fetch messages when expanded
  $effect(() => {
    if (isExpanded && session.id !== lastFetchedSessionId) {
      fetchMessages();
    }
  });

  // Auto-refresh messages for any non-terminal session (skip if focused - sessionMessages store handles that)
  const isTerminal = $derived(
    ['completed', 'failed', 'cancelled'].includes(session.status)
  );

  $effect(() => {
    if (isExpanded && !isTerminal && !isActive) {
      const interval = setInterval(fetchMessages, 2000);
      return () => clearInterval(interval);
    }
  });

  async function fetchMessages() {
    if (messagesLoading) return;
    try {
      messagesLoading = true;
      const data = await api.getSessionConversation(session.id);
      messages = data.messages;
      lastFetchedSessionId = session.id;
    } catch (e) {
      console.error('Failed to load session messages:', e);
    } finally {
      messagesLoading = false;
    }
  }

  // Configure marked for terminal aesthetic
  marked.setOptions({
    breaks: true,
    gfm: true
  });

  // Determine the border/accent color
  const sessionColor = $derived(session.color || 'var(--term-cyan)');

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
  <!-- Header (always visible) -->
  <div class="flex w-full items-center justify-between gap-2 px-3 py-2">
    <button
      type="button"
      class="flex min-w-0 flex-1 items-center gap-2 text-left hover:opacity-80"
      onclick={handleClick}
    >
      <!-- Color indicator -->
      <span
        class="h-2 w-2 shrink-0 rounded-full"
        style="background-color: {sessionColor};"
      ></span>

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

    <div class="flex shrink-0 items-center gap-2">
      <!-- Zoom button -->
      <button
        type="button"
        class="text-xs text-term-fg-muted hover:text-term-accent"
        onclick={handleZoom}
        aria-label="Zoom into session"
      >
        <svg xmlns="http://www.w3.org/2000/svg" class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
          <path stroke-linecap="round" stroke-linejoin="round" d="M4 8V4m0 0h4M4 4l5 5m11-1V4m0 0h-4m4 0l-5 5M4 16v4m0 0h4m-4 0l5-5m11 5l-5-5m5 5v-4m0 4h-4" />
        </svg>
      </button>

      <!-- Collapse/expand indicator -->
      <svg
        xmlns="http://www.w3.org/2000/svg"
        class="h-4 w-4 text-term-fg-muted transition-transform {isExpanded ? 'rotate-180' : ''}"
        fill="none"
        viewBox="0 0 24 24"
        stroke="currentColor"
        stroke-width="2"
      >
        <path stroke-linecap="round" stroke-linejoin="round" d="M19 9l-7 7-7-7" />
      </svg>
    </div>
  </div>

  <!-- Collapsed summary -->
  {#if !isExpanded && session.summary}
    <div class="border-t border-term-border px-3 py-2 text-xs text-term-fg-muted">
      {session.summary}
    </div>
  {/if}

  <!-- Expanded content: session messages as full message bubbles -->
  {#if isExpanded}
    <div class="border-t border-term-border">
      {#if messages.length === 0}
        <div class="px-3 py-2 text-xs text-term-fg-muted italic">
          {#if session.status === 'pending'}
            Session starting...
          {:else}
            No messages yet
          {/if}
        </div>
      {:else}
        <div class="space-y-2 py-2">
          {#each messages as msg (msg.id)}
            {@const isUser = msg.role === 'user'}
            {@const htmlContent = marked.parse(msg.content)}
            <div
              class="message w-full border-l-2 px-3 py-2 {isUser
                ? 'border-term-accent-alt bg-transparent'
                : 'border-term-accent bg-term-bg'}"
            >
              <div class="flex flex-col gap-1">
                <span class="text-xs {isUser ? 'text-term-accent-alt' : 'text-term-accent'}">
                  {isUser ? '$ user@session' : '> agent@session'}
                </span>
                <div class="prose-terminal text-sm text-term-fg">
                  {@html htmlContent}
                </div>
                <time class="text-xs text-term-fg-muted">
                  {new Date(msg.created_at).toLocaleTimeString()}
                </time>
              </div>
            </div>
          {/each}
        </div>
      {/if}

      <!-- Processing indicator -->
      {#if isRunning && messages.length > 0}
        <div class="flex items-center gap-2 border-t border-term-border px-3 py-2 text-xs text-term-cyan">
          <span class="h-3 w-3 animate-spin rounded-full border border-current border-t-transparent"></span>
          <span>Processing...</span>
        </div>
      {/if}

      <!-- Attention indicator -->
      {#if needsAttention}
        <div class="flex items-center gap-1 border-t border-term-border px-3 py-2 text-xs text-term-magenta">
          <span class="animate-pulse">*</span>
          <span>Waiting for your input</span>
        </div>
      {/if}
    </div>
  {/if}
</div>

<style>
  /* Terminal-styled markdown for session messages */
  .prose-terminal :global(p) {
    margin: 0 0 0.5em 0;
  }
  .prose-terminal :global(p:last-child) {
    margin-bottom: 0;
  }
  .prose-terminal :global(code) {
    background: var(--term-bg);
    border: 1px solid var(--term-border);
    padding: 0.125em 0.375em;
    font-size: 0.9em;
    word-break: break-word;
  }
  .prose-terminal :global(pre) {
    background: var(--term-bg);
    border: 1px solid var(--term-border);
    padding: 0.75em;
    margin: 0.5em 0;
    overflow-x: hidden;
    white-space: pre-wrap;
    word-break: break-word;
  }
  .prose-terminal :global(pre code) {
    background: none;
    border: none;
    padding: 0;
  }
  .prose-terminal :global(ul),
  .prose-terminal :global(ol) {
    margin: 0.5em 0;
    padding-left: 1.5em;
  }
  .prose-terminal :global(li) {
    margin: 0.25em 0;
  }
  .prose-terminal :global(ul) {
    list-style-type: disc;
  }
  .prose-terminal :global(ol) {
    list-style-type: decimal;
  }
  .prose-terminal :global(strong) {
    color: var(--term-accent);
    font-weight: 600;
  }
  .prose-terminal :global(a) {
    color: var(--term-info);
    text-decoration: underline;
  }
</style>
