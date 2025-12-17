<script lang="ts">
  import { conversationStore } from '$lib/stores/conversation';
  import { api } from '$lib/api';
  import MessageBubble from './MessageBubble.svelte';
  import InputBar from './InputBar.svelte';

  let { messages, isLoading } = $derived($conversationStore);

  async function handleSendMessage(event: CustomEvent<{ message: string }>) {
    const userMessage = event.detail.message;

    conversationStore.setLoading(true);

    try {
      const response = await api.sendMessage({
        message: userMessage,
        conversation_id: $conversationStore.currentConversation?.id
      });

      // Add user message
      conversationStore.addMessage({
        id: `temp-${Date.now()}`,
        conversation_id: response.conversation_id,
        role: 'user',
        content: userMessage,
        created_at: new Date().toISOString()
      });

      // Add assistant message
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
  <div class="flex-1 overflow-y-auto p-4 space-y-4">
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
      <div class="flex justify-center">
        <div class="text-neutral-400">Thinking...</div>
      </div>
    {/if}
  </div>

  <!-- Input -->
  <div class="border-t border-neutral-200 p-4">
    <InputBar on:send={handleSendMessage} disabled={isLoading} />
  </div>
</div>
