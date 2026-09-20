<script lang="ts">
  import { onMount } from 'svelte';
  import { conversationStore } from '$lib/stores/conversation';
  import { projects } from '$lib/stores/projects';
  import { sessions } from '$lib/stores/sessions';
  import { navigationContext, currentSession, isMainContext } from '$lib/stores/navigationContext';
  import { allSessionMessages } from '$lib/stores/sessionMessages';
  import { api, type MainThreadInfo } from '$lib/api';
  import ConversationView from './ConversationView.svelte';
  import NativeIdentityStrip from './NativeIdentityStrip.svelte';

  // Native main thread (MAIN_THREAD_MODE=native): a Claude session under Herdr whose window
  // Mainloop rotates. The reply is mirrored from the native journal, so we poll for it.
  let mainThread = $state<MainThreadInfo | null>(null);

  let { messages, isLoading } = $derived($conversationStore);

  // Start polling for all session messages
  $effect(() => {
    allSessionMessages.startPolling(3000);
    return () => allSessionMessages.stopPolling();
  });

  // Refresh session messages when sessions list changes (fixes race condition on page load)
  $effect(() => {
    const activeSessions = $sessions.sessions.filter(
      (s) => !['completed', 'failed', 'cancelled'].includes(s.status)
    );
    if (activeSessions.length > 0) {
      allSessionMessages.refreshAll();
    }
  });

  onMount(async () => {
    try {
      mainThread = await api.getMainThread();
      if (mainThread.mode === 'native' && mainThread.conversation_id) {
        const { conversation, messages } = await api.getConversation(mainThread.conversation_id);
        conversationStore.setConversation(conversation, messages);
        return;
      }
    } catch (error) {
      console.error('Failed to load main thread info:', error);
    }
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

      if (response.pending) {
        // Native main thread: the send is ledgered; poll until the turn completes.
        await pollNativeReply(response.conversation_id);
        sessions.fetchSessions();
        mainThread = await api.getMainThread();
        return;
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

  async function pollNativeReply(conversationId: string) {
    conversationStore.setLoading(true);
    for (let i = 0; i < 180; i++) {
      await new Promise((r) => setTimeout(r, 2000));
      const { messages: fresh } = await api.getConversation(conversationId);
      conversationStore.setMessages(fresh);
      const info = await api.getMainThread();
      mainThread = info;
      if (info.native && !info.native.turn_in_flight) return;
    }
  }

  async function sendSessionMessage(userMessage: string) {
    const session = $currentSession;
    if (!session) return;

    // Optimistic: Add user message immediately to session messages
    allSessionMessages.addOptimistic(session.id, {
      id: `temp-${Date.now()}`,
      conversation_id: session.conversation_id,
      role: 'user',
      content: userMessage,
      created_at: new Date().toISOString()
    });

    try {
      await api.sendSessionMessage(session.id, userMessage);
      // Refresh session status and messages
      sessions.fetchSessions();
      // Force immediate refresh to get real message ID and any quick response
      allSessionMessages.loadSession(session.id);
    } catch (error) {
      console.error('Failed to send session message:', error);
    }
  }
</script>

{#if mainThread?.mode === 'native' && mainThread.session_id}
  <NativeIdentityStrip sessionId={mainThread.session_id} />
  <div
    class="border-term-border text-term-fg-muted border-b px-4 py-1 font-mono text-xs"
    data-testid="topic-index"
  >
    topics:
    {#each mainThread.topics as t (t.name)}
      <span class="mr-3" data-testid="topic-line"
        >{t.name}{t.status_line ? ` (${t.status_line})` : ''} [{t.pending} pending]</span
      >
    {:else}
      <span>none yet</span>
    {/each}
  </div>
{/if}

<!-- Always show main thread - sessions appear inline -->
<ConversationView
  {messages}
  {isLoading}
  onSendMessage={handleSendMessage}
  placeholder={$currentSession ? `Reply to ${$currentSession.title}...` : 'Enter command...'}
  emptyStateTitle="$ mainloop --help"
  emptyStateMessage="Start a conversation to begin"
/>
