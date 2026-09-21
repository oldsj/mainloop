<script lang="ts">
  import { onMount } from 'svelte';
  import { conversationStore } from '$lib/stores/conversation';
  import { projects } from '$lib/stores/projects';
  import { sessions } from '$lib/stores/sessions';
  import { navigationContext, currentSession, isMainContext } from '$lib/stores/navigationContext';
  import { allSessionMessages } from '$lib/stores/sessionMessages';
  import { api, SendError, type MainThreadInfo } from '$lib/api';
  import { draftMessage } from '$lib/stores/draftMessage';
  import { connection } from '$lib/stores/connection';
  import { visibleMessages } from '$lib/messages';
  import ConversationView from './ConversationView.svelte';
  import MainThreadHeader from './MainThreadHeader.svelte';

  // Native main thread (MAIN_THREAD_MODE=native): a Claude session under Herdr whose window
  // Mainloop rotates. The reply is mirrored from the native journal, so we poll for it.
  let mainThread = $state<MainThreadInfo | null>(null);
  let sendError = $state<string | null>(null);

  let { messages: allMessages, isLoading } = $derived($conversationStore);
  const native = $derived(mainThread?.mode === 'native');
  // Protocol traffic (the pre-cut turn) is not a conversation the user had.
  const messages = $derived(native ? visibleMessages(allMessages) : allMessages);
  // The main thread takes one message at a time; say so instead of letting a send fail.
  const busy = $derived(
    native && !!(mainThread?.native?.turn_in_flight || mainThread?.native?.rotating)
  );
  const offline = $derived($connection.status === 'offline');
  const placeholder = $derived(
    offline
      ? 'Backend unreachable…'
      : $currentSession
        ? `Reply to ${$currentSession.title}...`
        : mainThread?.native?.rotating
          ? 'Resetting the context window…'
          : busy
            ? 'Working…'
            : 'Enter command...'
  );

  // Keep the main thread live without a send: child reports and rotations arrive on their own.
  $effect(() => {
    if (!native) return;
    let stopped = false;
    const tick = async () => {
      if (stopped || $conversationStore.isLoading) return;
      try {
        const info = await api.getMainThread();
        mainThread = info;
        if (info.conversation_id) {
          const { messages: fresh } = await api.getConversation(info.conversation_id);
          if (!stopped) conversationStore.setMessages(fresh);
        }
      } catch (error) {
        console.error('Main thread refresh failed:', error);
      }
    };
    const timer = setInterval(tick, 4000);
    return () => {
      stopped = true;
      clearInterval(timer);
    };
  });

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

  // Whether the initial load finished. Until it has, an empty thread means "not loaded", not
  // "nothing here", so the load is retried when the backend becomes reachable.
  let loaded = $state(false);
  let loadInFlight = false;

  async function loadInitial() {
    if (loaded || loadInFlight) return;
    loadInFlight = true;
    try {
      try {
        mainThread = await api.getMainThread();
        if (mainThread.mode === 'native' && mainThread.conversation_id) {
          const { conversation, messages } = await api.getConversation(mainThread.conversation_id);
          conversationStore.setConversation(conversation, messages);
          loaded = true;
          return;
        }
      } catch (error) {
        console.error('Failed to load main thread info:', error);
        return;
      }
      // Load the most recent conversation on startup
      const { conversations } = await api.listConversations();
      if (conversations.length > 0) {
        // Load the most recent conversation (already sorted by updated_at desc)
        const latest = conversations[0];
        const { conversation, messages } = await api.getConversation(latest.id);
        conversationStore.setConversation(conversation, messages);
      }
      loaded = true;
    } catch (error) {
      console.error('Failed to load conversation:', error);
    } finally {
      loadInFlight = false;
    }
  }

  onMount(loadInitial);

  // Retry a failed initial load as soon as the backend is reachable again.
  $effect(() => {
    if ($connection.status === 'online') void loadInitial();
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
    const tempId = `temp-${Date.now()}`;
    sendError = null;
    conversationStore.addMessage({
      id: tempId,
      conversation_id: currentConversationId || 'pending',
      role: 'user',
      content: userMessage,
      created_at: new Date().toISOString()
    });

    conversationStore.setLoading(true);

    // Once the backend has accepted the message it is delivered; a later failure (e.g. while
    // waiting for the reply) must not roll it back, or the user would send it twice.
    let accepted = false;
    try {
      const response = await api.sendMessage({
        message: userMessage,
        conversation_id: currentConversationId
      });
      accepted = true;

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
      // Delivered, but the follow-up failed: keep the message; the live refresh (and the
      // connection banner) take it from here.
      if (accepted) return;
      // Not delivered: take the optimistic bubble back, keep the text, and say why.
      conversationStore.setMessages($conversationStore.messages.filter((m) => m.id !== tempId));
      draftMessage.set(userMessage);
      sendError =
        error instanceof SendError && (error.status === 409 || error.status === 0)
          ? `${error.message} Your message is back in the box.`
          : 'Could not send the message. Your message is back in the box.';
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

<div class="flex h-full min-h-0 flex-col">
  {#if native && mainThread}
    <MainThreadHeader info={mainThread} />
  {/if}

  <!-- Always show main thread. Native mode: child sessions live in the side list, not inline. -->
  <div class="min-h-0 flex-1">
    <ConversationView
      {messages}
      {isLoading}
      onSendMessage={handleSendMessage}
      {placeholder}
      showInlineSessions={!native}
      error={sendError}
      inputDisabled={busy || offline}
      onDismissError={() => (sendError = null)}
      emptyStateTitle={loaded ? '$ mainloop --help' : '$ connecting'}
      emptyStateMessage={loaded ? 'Start a conversation to begin' : 'Waiting for the backend…'}
    />
  </div>
</div>
