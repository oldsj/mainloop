<script lang="ts">
  import { onMount } from 'svelte';
  import { conversationStore } from '$lib/stores/conversation';
  import { projects } from '$lib/stores/projects';
  import { sessions } from '$lib/stores/sessions';
  import { navigationContext, currentSession, isMainContext } from '$lib/stores/navigationContext';
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

    // Route to session or main thread based on context
    if (!$isMainContext && $currentSession) {
      await sendSessionMessage(userMessage);
    } else {
      await sendMainThreadMessage(userMessage);
    }
  }

  async function sendMainThreadMessage(userMessage: string) {
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

      // Check if a session was spawned (no assistant message)
      if (response.spawned_session_id) {
        // Reload conversation to get real message IDs (needed for anchor matching)
        const { messages: freshMessages } = await api.getConversation(response.conversation_id);
        conversationStore.setMessages(freshMessages);
        // Refresh sessions to get the new one
        await sessions.fetchSessions();
        // Auto-switch to the spawned session
        navigationContext.switchToSession(response.spawned_session_id);
      } else if (response.message) {
        // Add assistant response (normal case)
        conversationStore.addMessage(response.message);
      }

      // Refresh projects in case a task was spawned
      projects.fetchProjects();
    } catch (error) {
      console.error('Failed to send message:', error);
    } finally {
      conversationStore.setLoading(false);
    }
  }

  async function sendSessionMessage(userMessage: string) {
    const session = $currentSession;
    if (!session) return;

    try {
      await api.sendSessionMessage(session.id, userMessage);
      // Refresh session status - SessionBlock will auto-refresh its messages
      sessions.fetchSessions();
    } catch (error) {
      console.error('Failed to send session message:', error);
    }
  }
</script>

<!-- Always show main thread - sessions appear inline -->
<ConversationView
  {messages}
  {isLoading}
  onSendMessage={handleSendMessage}
  placeholder={$currentSession ? `Reply to ${$currentSession.title}...` : 'Enter command...'}
  emptyStateTitle="$ mainloop --help"
  emptyStateMessage="Start a conversation to begin"
/>
