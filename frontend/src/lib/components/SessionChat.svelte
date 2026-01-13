<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type Message, type Session } from '$lib/api';
  import ConversationView from './ConversationView.svelte';

  let { sessionId }: { sessionId: string } = $props();

  let session = $state<Session | null>(null);
  let messages = $state<Message[]>([]);
  let isLoading = $state(false);
  let error = $state<string | null>(null);

  onMount(async () => {
    await loadSession();
  });

  async function loadSession() {
    try {
      const result = await api.getSessionConversation(sessionId);
      session = result.session;
      messages = result.messages;
    } catch (e) {
      console.error('Failed to load session:', e);
      error = 'Failed to load session';
    }
  }

  async function handleSendMessage(detail: { message: string }) {
    if (!session) return;

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
      // Reload messages to get the full response
      await loadSession();
    } catch (e) {
      console.error('Failed to send message:', e);
      error = 'Failed to send message';
    } finally {
      isLoading = false;
    }
  }
</script>

{#if error}
  <div class="flex h-full items-center justify-center text-term-red">
    <p>{error}</p>
  </div>
{:else}
  <ConversationView
    {messages}
    {isLoading}
    onSendMessage={handleSendMessage}
    placeholder="Send a message to this session..."
    emptyStateTitle="$ session --start"
    emptyStateMessage="This session's conversation will appear here"
  />
{/if}
