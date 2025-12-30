<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { conversationStore } from '$lib/stores/conversation';
  import { api } from '$lib/api';
  import MessageBubble from './MessageBubble.svelte';
  import InputBar from './InputBar.svelte';

  let { messages, isLoading } = $derived($conversationStore);
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

  onMount(async () => {
    // Load the most recent conversation on startup
    try {
      const { conversations } = await api.listConversations();
      if (conversations.length > 0) {
        // Load the most recent conversation (already sorted by updated_at desc)
        const latest = conversations[0];
        const { conversation, messages } = await api.getConversation(latest.id);
        conversationStore.setConversation(conversation, messages);
      }
    } catch (error) {
      console.error('Failed to load conversation:', error);
    }
  });

  async function handleSendMessage(detail: { message: string }) {
    const userMessage = detail.message;
    const currentConversationId = $conversationStore.currentConversation?.id;

    // Optimistic: Add user message immediately
    conversationStore.addMessage({
      id: `temp-${Date.now()}`,
      conversation_id: currentConversationId || 'pending',
      role: 'user',
      content: userMessage,
      created_at: new Date().toISOString()
    });

    conversationStore.setLoading(true);

    try {
      const response = await api.sendMessage({
        message: userMessage,
        conversation_id: currentConversationId
      });

      // Update conversation ID if this was the first message
      if (!currentConversationId) {
        conversationStore.setCurrentConversation({
          id: response.conversation_id,
          user_id: '',
          title: null,
          created_at: new Date().toISOString(),
          updated_at: new Date().toISOString()
        });
      }

      // Add assistant response (now returned synchronously)
      conversationStore.addMessage(response.message);
    } catch (error) {
      console.error('Failed to send message:', error);
    } finally {
      conversationStore.setLoading(false);
    }
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
        <p class="text-term-accent">$ mainloop --help</p>
        <p class="mt-2">Start a conversation to begin</p>
        <p class="animate-cursor text-term-accent">_</p>
      </div>
    {:else}
      {#each messages as message (message.id)}
        <MessageBubble {message} />
      {/each}
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
    <InputBar onsend={handleSendMessage} disabled={isLoading} />
  </div>
</div>
