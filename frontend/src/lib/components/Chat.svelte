<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { conversationStore } from '$lib/stores/conversation';
  import { api } from '$lib/api';
  import MessageBubble from './MessageBubble.svelte';
  import InputBar from './InputBar.svelte';

  let { messages, isLoading } = $derived($conversationStore);
  let messagesContainer: HTMLDivElement;

  // Auto-scroll to bottom when messages change or loading state changes
  $effect(() => {
    // Track these values to trigger effect
    messages;
    isLoading;

    // Scroll after DOM updates
    tick().then(() => {
      if (messagesContainer) {
        messagesContainer.scrollTop = messagesContainer.scrollHeight;
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

<div class="flex h-full flex-col">
  <!-- Messages -->
  <div bind:this={messagesContainer} class="flex-1 overflow-y-auto p-4 space-y-4">
    {#if messages.length === 0}
      <div class="flex h-full items-center justify-center text-neutral-400">
        <p>Start a conversation</p>
      </div>
    {:else}
      {#each messages as message (message.id)}
        <MessageBubble {message} />
      {/each}
    {/if}

    {#if isLoading}
      <div class="flex justify-start">
        <div class="flex items-center gap-2 rounded-2xl bg-neutral-100 px-4 py-3">
          <div class="flex gap-1">
            <span class="h-2 w-2 animate-bounce rounded-full bg-neutral-400" style="animation-delay: 0ms"></span>
            <span class="h-2 w-2 animate-bounce rounded-full bg-neutral-400" style="animation-delay: 150ms"></span>
            <span class="h-2 w-2 animate-bounce rounded-full bg-neutral-400" style="animation-delay: 300ms"></span>
          </div>
          <span class="text-sm text-neutral-500">Thinking...</span>
        </div>
      </div>
    {/if}
  </div>

  <!-- Input -->
  <div class="border-t border-neutral-200 p-4">
    <InputBar onsend={handleSendMessage} disabled={isLoading} />
  </div>
</div>
