<script lang="ts">
  import { tick } from 'svelte';
  import type { Message, Session } from '$lib/api';
  import { sessions } from '$lib/stores/sessions';
  import { navigationContext, currentSession } from '$lib/stores/navigationContext';
  import { allSessionMessagesFlat } from '$lib/stores/sessionMessages';
  import { messageTime } from '$lib/time';
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
    context = 'main',
    error = null,
    inputDisabled = false,
    onDismissError
  }: {
    messages: Message[];
    isLoading: boolean;
    onSendMessage: (detail: { message: string }) => Promise<void>;
    placeholder?: string;
    emptyStateTitle?: string;
    emptyStateMessage?: string;
    showInlineSessions?: boolean;
    context?: string;
    /** A send that was rejected; shown above the input, not lost in the console. */
    error?: string | null;
    /** Disable sending without implying a running turn (e.g. the window is rotating). */
    inputDisabled?: boolean;
    onDismissError?: () => void;
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
    return $sessions.sessions.filter((s) => !s.anchor_message_id);
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
  // Follow new messages while the reader is at the bottom (true on first load); scrolling up
  // releases it. Measuring after the DOM grew would always look "far from the bottom".
  let stickToBottom = true;

  function distanceFromBottom() {
    const { scrollTop, scrollHeight, clientHeight } = messagesContainer;
    return scrollHeight - scrollTop - clientHeight;
  }

  function checkScrollPosition() {
    if (!messagesContainer) return;
    const distance = distanceFromBottom();
    stickToBottom = distance < 150;
    showScrollButton = distance > 100;
  }

  function scrollToBottom() {
    if (messagesContainer) {
      stickToBottom = true;
      messagesContainer.scrollTo({
        top: messagesContainer.scrollHeight,
        behavior: 'smooth'
      });
    }
  }

  $effect(() => {
    // Track these values to trigger effect
    messages;
    isLoading;
    $allSessionMessagesFlat;

    if (stickToBottom) {
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

<div class="bg-term-bg relative flex h-full min-h-0 flex-col">
  <!-- Messages (the only scrolling region; the input below never scrolls away) -->
  <div
    bind:this={messagesContainer}
    onscroll={checkScrollPosition}
    class="min-h-0 flex-1 space-y-2 overflow-y-auto p-4"
  >
    {#if messages.length === 0}
      <div class="text-term-fg-muted flex h-full flex-col items-center justify-center">
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
            class="hover:bg-term-bg-secondary/50 my-1 ml-10 flex w-[calc(100%-2.5rem)] items-start gap-2 border-l-4 px-3 py-2 text-left transition-colors {isActiveSession
              ? 'bg-term-bg-secondary/50 ring-term-accent ring-1'
              : 'bg-term-bg-secondary/30'}"
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
                  {messageTime(item.message.created_at)}
                </time>
              </div>
              <div class="text-term-fg-muted mt-1 truncate text-sm">
                {preview}{#if isLong}...{/if}
              </div>
            </div>
            <span class="text-term-fg-muted shrink-0 text-xs">→</span>
          </button>
        {/if}
      {/each}

      <!-- Sessions without anchors (show at bottom) -->
      {#if showInlineSessions && unanchoredSessions().length > 0}
        <div class="border-term-border mt-4 border-t pt-4">
          <div class="text-term-fg-muted mb-2 text-xs">Active Sessions</div>
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
        {#if ['pending', 'active', 'implementing'].includes($currentSession.status)}
          <div
            class="my-1 ml-10 flex items-center gap-2 border-l-4 px-3 py-2 text-xs"
            style="border-color: {sessionColor}; color: {sessionColor};"
          >
            <span
              class="h-3 w-3 animate-spin rounded-full border border-current border-t-transparent"
            ></span>
            <span>{$currentSession.title} processing...</span>
          </div>
        {/if}
      {/if}
    {/if}

    {#if isLoading}
      <div
        class="border-term-accent bg-term-bg-secondary flex w-full flex-col gap-1 border-l-2 px-3 py-2 md:px-4"
      >
        <span class="text-term-accent text-xs md:text-sm">
          claude@{context}$
        </span>
        <div class="flex items-center gap-2">
          <span class="text-term-fg-muted text-sm">processing</span>
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
      class="border-term-border bg-term-bg-secondary text-term-fg-muted hover:border-term-accent hover:text-term-accent absolute right-4 bottom-24 flex h-10 w-10 items-center justify-center border transition-colors"
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

  <!-- Input: always visible at the bottom -->
  <div
    class="border-term-border bg-term-bg shrink-0 border-t p-3 pb-[max(0.75rem,env(safe-area-inset-bottom))] md:p-4"
  >
    {#if error}
      <div
        class="border-term-red/60 bg-term-red/10 text-term-red mb-2 flex items-start justify-between gap-2 border px-3 py-2 text-sm"
        role="alert"
        data-testid="send-error"
      >
        <span>{error}</span>
        {#if onDismissError}
          <button type="button" class="shrink-0 hover:underline" onclick={onDismissError}
            >dismiss</button
          >
        {/if}
      </div>
    {/if}
    <InputBar onsend={handleSend} disabled={isLoading || inputDisabled} {placeholder} />
  </div>
</div>
