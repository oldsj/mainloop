<script lang="ts">
  import { tick } from 'svelte';
  import type { Message, Session } from '$lib/api';
  import { sessions } from '$lib/stores/sessions';
  import { navigationContext } from '$lib/stores/navigationContext';
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
    showInlineSessions = true
  }: {
    messages: Message[];
    isLoading: boolean;
    onSendMessage: (detail: { message: string }) => Promise<void>;
    placeholder?: string;
    emptyStateTitle?: string;
    emptyStateMessage?: string;
    showInlineSessions?: boolean;
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

  // Auto-scroll to bottom when messages change or loading state changes
  $effect(() => {
    // Track these values to trigger effect
    messages;
    isLoading;

    // Scroll after DOM updates
    tick().then(() => {
      if (messagesContainer) {
        messagesContainer.scrollTop = messagesContainer.scrollHeight;
        showScrollButton = false;
      }
    });
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
      {#each messages as message (message.id)}
        <MessageBubble {message} />

        <!-- Inline sessions anchored to this message -->
        {#if showInlineSessions}
          {@const anchored = sessionsByAnchor().get(message.id) || []}
          {#each anchored as session (session.id)}
            <SessionBlock
              {session}
              isActive={$navigationContext.currentContext === session.id}
              onSelect={() => navigationContext.switchToSession(session.id)}
            />
          {/each}
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
    {/if}

    {#if isLoading}
      <div
        class="flex w-full flex-col gap-1 border-l-2 border-term-accent bg-term-bg-secondary px-3 py-2 md:flex-row md:items-center md:gap-3 md:px-4"
      >
        <span class="text-xs text-term-accent md:text-sm">
          >
          <span class="hidden md:inline">claude@mainloop</span>
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
