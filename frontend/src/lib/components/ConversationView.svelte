<script lang="ts">
  import { tick } from 'svelte';
  import type { Message, Session } from '$lib/api';
  import { sessions } from '$lib/stores/sessions';
  import { navigationContext, currentSession } from '$lib/stores/navigationContext';
  import { allSessionMessagesFlat } from '$lib/stores/sessionMessages';
  import { marked } from 'marked';
  import MessageBubble from './MessageBubble.svelte';
  import InputBar from './InputBar.svelte';
  import SessionBlock from './SessionBlock.svelte';

  let {
    messages = [],
    isLoading = false,
    onSendMessage,
    placeholder = 'Enter command...',
    emptyStateTitle = '$ mainloop --help',
    emptyStateMessage = 'Start a conversation to begin',
    showInlineSessions = true,
    context = 'main'
  }: {
    messages: Message[];
    isLoading: boolean;
    onSendMessage: (detail: { message: string }) => Promise<void>;
    placeholder?: string;
    emptyStateTitle?: string;
    emptyStateMessage?: string;
    showInlineSessions?: boolean;
    context?: string;
  } = $props();

  // Map of anchor_message_id -> sessions for inline rendering
  const sessionsByAnchor = $derived(() => {
    if (!showInlineSessions) return new Map<string, Session[]>();

    const map = new Map<string, Session[]>();
    for (const session of $sessions.sessions) {
      if (session.anchor_message_id) {
        const existing = map.get(session.anchor_message_id) || [];
        existing.push(session);
        map.set(session.anchor_message_id, existing);
      }
    }
    return map;
  });

  // Sessions without anchors (show at bottom of conversation)
  const unanchoredSessions = $derived(() => {
    if (!showInlineSessions) return [];
    return $sessions.sessions.filter(s => !s.anchor_message_id);
  });

  // Configure marked for terminal aesthetic
  marked.setOptions({
    breaks: true,
    gfm: true
  });

  // Unified timeline: merge main messages with session messages (when focused)
  type TimelineItem =
    | { type: 'message'; message: Message }
    | { type: 'session-anchor'; message: Message; sessions: Session[] }
    | { type: 'thread-reply'; message: Message; session: Session };

  const timeline = $derived(() => {
    const items: TimelineItem[] = [];

    // Add main thread messages (with session anchors)
    for (const message of messages) {
      const anchored = sessionsByAnchor().get(message.id) || [];
      if (anchored.length > 0) {
        items.push({ type: 'session-anchor', message, sessions: anchored });
      } else {
        items.push({ type: 'message', message });
      }
    }

    // Add ALL session messages as thread replies (only in main thread view)
    if (showInlineSessions) {
      for (const { message, session } of $allSessionMessagesFlat) {
        items.push({ type: 'thread-reply', message, session });
      }
    }

    // Sort everything by timestamp
    items.sort((a, b) => {
      const timeA = new Date(a.message.created_at).getTime();
      const timeB = new Date(b.message.created_at).getTime();
      return timeA - timeB;
    });

    return items;
  });

  let messagesContainer: HTMLDivElement;
  let showScrollButton = $state(false);

  // Check if scrolled to bottom
  function checkScrollPosition() {
    if (!messagesContainer) return;
    const { scrollTop, scrollHeight, clientHeight } = messagesContainer;
    const distanceFromBottom = scrollHeight - scrollTop - clientHeight;
    showScrollButton = distanceFromBottom > 100;
  }

  function scrollToBottom() {
    if (messagesContainer) {
      messagesContainer.scrollTo({
        top: messagesContainer.scrollHeight,
        behavior: 'smooth'
      });
    }
  }

  // Auto-scroll to bottom when messages change (only if already near bottom)
  $effect(() => {
    // Track these values to trigger effect
    messages;
    isLoading;
    $allSessionMessagesFlat;

    // Check if user was already near bottom before updates
    const wasNearBottom = messagesContainer
      ? messagesContainer.scrollHeight - messagesContainer.scrollTop - messagesContainer.clientHeight < 150
      : true;

    // Only auto-scroll if user was already at/near bottom
    if (wasNearBottom) {
      tick().then(() => {
        if (messagesContainer) {
          messagesContainer.scrollTop = messagesContainer.scrollHeight;
          showScrollButton = false;
        }
      });
    }
  });

  async function handleSend(detail: { message: string }) {
    await onSendMessage(detail);
  }
</script>

<div class="relative flex h-full flex-col bg-term-bg">
  <!-- Messages -->
  <div
    bind:this={messagesContainer}
    onscroll={checkScrollPosition}
    class="flex-1 space-y-2 overflow-y-auto p-4"
  >
    {#if messages.length === 0}
      <div class="flex h-full flex-col items-center justify-center text-term-fg-muted">
        <p class="text-term-accent">{emptyStateTitle}</p>
        <p class="mt-2">{emptyStateMessage}</p>
        <p class="animate-cursor text-term-accent">_</p>
      </div>
    {:else}
      {#each timeline() as item (item.type === 'thread-reply' ? `thread-${item.message.id}` : item.message.id)}
        {#if item.type === 'message'}
          <MessageBubble message={item.message} {context} />
        {:else if item.type === 'session-anchor'}
          <MessageBubble message={item.message} {context} />
        {:else if item.type === 'thread-reply'}
          <!-- Thread reply notification (Slack-style "replied in thread") -->
          {@const sessionColor = item.session.color}
          {@const isUser = item.message.role === 'user'}
          {@const modelName = item.session.model || 'claude'}
          {@const preview = item.message.content.slice(0, 120)}
          {@const isLong = item.message.content.length > 120}
          {@const isActiveSession = $navigationContext.currentContext === item.session.id}
          <button
            type="button"
            class="my-1 ml-10 flex w-[calc(100%-2.5rem)] items-start gap-2 border-l-4 px-3 py-2 text-left transition-colors hover:bg-term-bg-secondary/50 {isActiveSession ? 'bg-term-bg-secondary/50 ring-1 ring-term-accent' : 'bg-term-bg-secondary/30'}"
            style="border-color: {sessionColor};"
            onclick={() => navigationContext.switchToSession(item.session.id)}
            ondblclick={() => navigationContext.zoomSession(item.session.id)}
          >
            <span
              class="mt-1 h-2 w-2 shrink-0 rounded-full"
              style="background-color: {sessionColor};"
            ></span>
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2 text-xs">
                <span class={isUser ? 'text-term-accent-alt' : 'text-term-accent'}>
                  {isUser ? 'user' : modelName}@{item.session.title}$
                </span>
                <span class="text-term-fg-muted">·</span>
                <time class="text-term-fg-muted">
                  {new Date(item.message.created_at).toLocaleTimeString()}
                </time>
              </div>
              <div class="mt-1 truncate text-sm text-term-fg-muted">
                {preview}{#if isLong}...{/if}
              </div>
            </div>
            <span class="shrink-0 text-xs text-term-fg-muted">→</span>
          </button>
        {/if}
      {/each}

      <!-- Sessions without anchors (show at bottom) -->
      {#if showInlineSessions && unanchoredSessions().length > 0}
        <div class="mt-4 border-t border-term-border pt-4">
          <div class="mb-2 text-xs text-term-fg-muted">Active Sessions</div>
          {#each unanchoredSessions() as session (session.id)}
            <SessionBlock
              {session}
              isActive={$navigationContext.currentContext === session.id}
              onSelect={() => navigationContext.switchToSession(session.id)}
            />
          {/each}
        </div>
      {/if}

      <!-- Session status indicator when focused -->
      {#if $currentSession}
        {@const sessionColor = $currentSession.color}
        {#if ['pending', 'active', 'planning', 'implementing'].includes($currentSession.status)}
          <div
            class="my-1 ml-10 flex items-center gap-2 border-l-4 px-3 py-2 text-xs"
            style="border-color: {sessionColor}; color: {sessionColor};"
          >
            <span class="h-3 w-3 animate-spin rounded-full border border-current border-t-transparent"></span>
            <span>{$currentSession.title} processing...</span>
          </div>
        {/if}
      {/if}
    {/if}

    {#if isLoading}
      <div
        class="flex w-full flex-col gap-1 border-l-2 border-term-accent bg-term-bg-secondary px-3 py-2 md:px-4"
      >
        <span class="text-xs text-term-accent md:text-sm">
          claude@{context}$
        </span>
        <div class="flex items-center gap-2">
          <span class="text-sm text-term-fg-muted">processing</span>
          <span class="animate-cursor text-term-accent">_</span>
        </div>
      </div>
    {/if}
  </div>

  <!-- Scroll to bottom button -->
  {#if showScrollButton}
    <button
      type="button"
      onclick={scrollToBottom}
      class="absolute bottom-24 right-4 flex h-10 w-10 items-center justify-center border border-term-border bg-term-bg-secondary text-term-fg-muted transition-colors hover:border-term-accent hover:text-term-accent"
      aria-label="Scroll to bottom"
    >
      <svg
        xmlns="http://www.w3.org/2000/svg"
        fill="none"
        viewBox="0 0 24 24"
        stroke-width="2"
        stroke="currentColor"
        class="h-5 w-5"
      >
        <path stroke-linecap="square" stroke-linejoin="miter" d="M19 14l-7 7m0 0l-7-7m7 7V3" />
      </svg>
    </button>
  {/if}

  <!-- Input -->
  <div class="border-t border-term-border p-4">
    <InputBar onsend={handleSend} disabled={isLoading} {placeholder} />
  </div>
</div>

<style>
  /* Terminal-styled markdown for thread messages */
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
