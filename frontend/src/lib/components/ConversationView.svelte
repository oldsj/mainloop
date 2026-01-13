<script lang="ts">
  import { tick } from 'svelte';
  import type { Message, Session } from '$lib/api';
  import { sessions } from '$lib/stores/sessions';
  import { navigationContext, currentSession } from '$lib/stores/navigationContext';
  import { currentSessionMessages } from '$lib/stores/sessionMessages';
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

  // Configure marked for terminal aesthetic
  marked.setOptions({
    breaks: true,
    gfm: true
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
    $currentSessionMessages;

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

      <!-- Inline thread replies when focused on a session -->
      {#if $currentSession && $currentSessionMessages.length > 0}
        {@const sessionColor = $currentSession.color || 'var(--term-cyan)'}
        <div
          class="mt-4 border-l-4 bg-term-bg-secondary/50"
          style="border-color: {sessionColor};"
        >
          <!-- Thread header -->
          <div class="flex items-center gap-2 border-b border-term-border px-3 py-2">
            <span
              class="h-2 w-2 rounded-full"
              style="background-color: {sessionColor};"
            ></span>
            <span class="text-xs text-term-fg-muted">
              Thread: <span style="color: {sessionColor};">{$currentSession.title}</span>
            </span>
          </div>

          <!-- Thread messages -->
          <div class="space-y-2 py-2">
            {#each $currentSessionMessages as msg (msg.id)}
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

          <!-- Session status indicator -->
          {#if ['pending', 'active', 'planning', 'implementing'].includes($currentSession.status)}
            <div class="flex items-center gap-2 border-t border-term-border px-3 py-2 text-xs text-term-cyan">
              <span class="h-3 w-3 animate-spin rounded-full border border-current border-t-transparent"></span>
              <span>Processing...</span>
            </div>
          {:else if ['waiting_on_user', 'waiting_questions', 'waiting_plan_review'].includes($currentSession.status)}
            <div class="flex items-center gap-1 border-t border-term-border px-3 py-2 text-xs text-term-magenta">
              <span class="animate-pulse">*</span>
              <span>Waiting for your input</span>
            </div>
          {/if}
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
