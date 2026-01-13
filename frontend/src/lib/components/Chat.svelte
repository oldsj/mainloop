<script lang="ts">
  import { onMount } from 'svelte';
  import { conversationStore } from '$lib/stores/conversation';
  import { projects } from '$lib/stores/projects';
  import { api } from '$lib/api';
  import ConversationView from './ConversationView.svelte';

  let { messages, isLoading } = $derived($conversationStore);

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

      // Refresh projects in case a task was spawned
      projects.fetchProjects();
    } catch (error) {
      console.error('Failed to send message:', error);
    } finally {
      conversationStore.setLoading(false);
    }
  }
</script>

<ConversationView
  {messages}
  {isLoading}
  onSendMessage={handleSendMessage}
  placeholder="Enter command..."
  emptyStateTitle="$ mainloop --help"
  emptyStateMessage="Start a conversation to begin"
/>
