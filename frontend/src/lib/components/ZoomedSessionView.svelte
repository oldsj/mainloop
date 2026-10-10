<script lang="ts">
  import { onMount } from 'svelte';
  import type { Session, Message } from '$lib/api';
  import { agentLabel } from '$lib/agentLabel';
  import { statusLabel } from '$lib/sessionStatus';
  import { api } from '$lib/api';
  import { navigationContext } from '$lib/stores/navigationContext';
  import { sessions } from '$lib/stores/sessions';
  import MessageBubble from './MessageBubble.svelte';
  import InputBar from './InputBar.svelte';

  let { sessionId }: { sessionId: string } = $props();

  let session = $state<Session | null>(null);
  let messages = $state<Message[]>([]);
  let isLoading = $state(false);
  let error = $state<string | null>(null);
  let messagesContainer: HTMLDivElement;

  // Fetch session data on mount and when sessionId changes
  $effect(() => {
    if (sessionId) {
      loadSessionData(sessionId);
    }
  });

  async function loadSessionData(id: string) {
    try {
      isLoading = true;
      error = null;
      const data = await api.getSessionConversation(id);
      session = data.session;
      messages = data.messages;
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed to load session';
      console.error('Failed to load session:', e);
    } finally {
      isLoading = false;
    }
  }

  async function handleSendMessage(detail: { message: string }) {
    if (!sessionId || !session) return;

    const userMessage = detail.message;

    // Optimistic: Add user message immediately
    messages = [
      ...messages,
      {
        id: `temp-${Date.now()}`,
        conversation_id: session.conversation_id,
        role: 'user',
        content: userMessage,
        created_at: new Date().toISOString()
      }
    ];

    isLoading = true;

    try {
      await api.sendSessionMessage(sessionId, userMessage);
      // Reload to get the response
      await loadSessionData(sessionId);
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed to send message';
      console.error('Failed to send session message:', e);
    } finally {
      isLoading = false;
    }
  }

  function handleExit() {
    navigationContext.exitZoom();
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      handleExit();
    }
  }

  // Auto-scroll to bottom when messages change
  $effect(() => {
    messages;
    if (messagesContainer) {
      messagesContainer.scrollTop = messagesContainer.scrollHeight;
    }
  });

  // Status styling
  const statusColors: Record<string, string> = {
    waiting_on_user: 'text-term-magenta',
    active: 'text-term-cyan',
    implementing: 'text-term-cyan',
    pending: 'text-term-yellow',
    completed: 'text-term-green',
    failed: 'text-term-red',
    cancelled: 'text-term-fg-muted'
  };

  const needsAttention = $derived(session?.needs_owner === true);
</script>

<svelte:window onkeydown={handleKeydown} />

<div class="bg-term-bg flex h-full flex-col">
  <!-- Header -->
  <div
    class="border-term-border flex items-center justify-between border-b px-4 py-3"
    style="border-left: 4px solid {session?.color};"
  >
    <div class="flex min-w-0 flex-1 items-center gap-3">
      <!-- Back button -->
      <button
        type="button"
        class="text-term-fg-muted hover:text-term-accent shrink-0"
        onclick={handleExit}
        aria-label="Exit zoom mode"
      >
        <svg
          xmlns="http://www.w3.org/2000/svg"
          class="h-5 w-5"
          fill="none"
          viewBox="0 0 24 24"
          stroke="currentColor"
          stroke-width="2"
        >
          <path stroke-linecap="round" stroke-linejoin="round" d="M10 19l-7-7m0 0l7-7m-7 7h18" />
        </svg>
      </button>

      <!-- Session info -->
      {#if session}
        <div class="min-w-0 flex-1">
          <div class="flex items-center gap-2">
            <span class="h-2 w-2 shrink-0 rounded-full" style="background-color: {session.color};"
            ></span>
            <h1 class="text-term-fg truncate text-lg font-medium">
              {session.title}
            </h1>
            <span class="shrink-0 text-xs {statusColors[session.status] || 'text-term-fg-muted'}">
              [{statusLabel(session.status, session.needs_owner)}]
            </span>
          </div>
          <p class="text-term-fg-muted truncate text-xs">
            {session.description}
          </p>
        </div>
      {:else}
        <span class="text-term-fg-muted">Loading session...</span>
      {/if}
    </div>

    <!-- Exit button -->
    <button
      type="button"
      class="border-term-border text-term-fg hover:border-term-accent hover:text-term-accent shrink-0 border px-3 py-1 text-xs"
      onclick={handleExit}
    >
      ESC to exit
    </button>
  </div>

  <!-- Messages -->
  <div bind:this={messagesContainer} class="flex-1 space-y-2 overflow-y-auto p-4">
    {#if error}
      <div class="text-term-red text-center">
        <p>Error: {error}</p>
        <button
          type="button"
          class="text-term-accent mt-2 text-sm hover:underline"
          onclick={() => loadSessionData(sessionId)}
        >
          Retry
        </button>
      </div>
    {:else if messages.length === 0}
      <div class="text-term-fg-muted flex h-full flex-col items-center justify-center">
        <p class="text-term-accent">$ session --view {session?.title || sessionId}</p>
        <p class="mt-2">
          {#if session?.status === 'pending'}
            Session starting...
          {:else}
            No messages yet
          {/if}
        </p>
      </div>
    {:else}
      {#each messages as message (message.id)}
        <MessageBubble
          {message}
          context={session?.title ?? 'session'}
          agentKind={session?.agent_kind}
        />
      {/each}
    {/if}

    {#if isLoading}
      {@const modelName = agentLabel(session?.agent_kind)}
      <div
        class="border-term-accent bg-term-bg-secondary flex w-full flex-col gap-1 border-l-2 px-3 py-2 md:px-4"
      >
        <span class="text-term-accent text-xs md:text-sm">
          {modelName}@{session?.title || 'session'}$
        </span>
        <div class="flex items-center gap-2">
          <span class="text-term-fg-muted text-sm">processing</span>
          <span class="animate-cursor text-term-accent">_</span>
        </div>
      </div>
    {/if}
  </div>

  <!-- Attention indicator -->
  {#if needsAttention}
    <div
      class="border-term-border bg-term-magenta/10 text-term-magenta flex items-center justify-center gap-2 border-t py-2"
    >
      <span class="animate-pulse">*</span>
      <span class="text-sm">This session is waiting for your input</span>
    </div>
  {/if}

  <!-- Input -->
  <div class="border-term-border border-t p-4" style="border-left: 4px solid {session?.color};">
    <InputBar
      onsend={handleSendMessage}
      disabled={isLoading || !session}
      placeholder="Reply to {session?.title || 'session'}..."
    />
  </div>
</div>
