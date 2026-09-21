<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type Message, type Session } from '$lib/api';
  import { connection } from '$lib/stores/connection';
  import { draftMessage } from '$lib/stores/draftMessage';
  import ConversationView from './ConversationView.svelte';

  let { sessionId }: { sessionId: string } = $props();

  let session = $state<Session | null>(null);
  let messages = $state<Message[]>([]);
  let isLoading = $state(false);
  // The last poll failed. Cleared by the next successful one; the messages already shown stay.
  let loadError = $state<string | null>(null);
  // The last send was not delivered. Stays until dismissed or the next send.
  let sendError = $state<string | null>(null);

  const offline = $derived($connection.status === 'offline');

  onMount(() => {
    loadSession();
    // Native agent replies arrive from the journal after the POST returns: keep reading.
    const timer = setInterval(loadSession, 2500);
    return () => clearInterval(timer);
  });

  async function loadSession() {
    try {
      const result = await api.getSessionConversation(sessionId);
      session = result.session;
      messages = result.messages;
      isLoading = session.status === 'active';
      loadError = null;
    } catch (e) {
      console.error('Failed to load session:', e);
      loadError = "Couldn't refresh this session. Retrying…";
    }
  }

  async function handleSendMessage(detail: { message: string }) {
    if (!session) return;

    const userMessage = detail.message;
    const tempId = `temp-${Date.now()}`;
    sendError = null;

    // Optimistic: Add user message immediately
    messages = [
      ...messages,
      {
        id: tempId,
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
      // Not delivered: take the optimistic bubble back, keep the text, and say so.
      messages = messages.filter((m) => m.id !== tempId);
      draftMessage.set(userMessage);
      sendError = 'Could not send the message. Your message is back in the box.';
    } finally {
      isLoading = false;
    }
  }
</script>

<ConversationView
  {messages}
  {isLoading}
  onSendMessage={handleSendMessage}
  placeholder={offline ? 'Backend unreachable…' : 'Message this session...'}
  emptyStateTitle="$ session --start"
  emptyStateMessage="This session's conversation will appear here"
  showInlineSessions={false}
  context={session?.title ?? 'session'}
  error={sendError ?? loadError}
  inputDisabled={offline}
  onDismissError={() => {
    sendError = null;
    loadError = null;
  }}
/>
