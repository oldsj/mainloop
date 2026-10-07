<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type Message, type NativeSessionInfo, type Session } from '$lib/api';
  import { deliveryNotices } from '$lib/delivery';
  import { createSendGuard, retryMessage } from '$lib/messageSend';
  import { connection } from '$lib/stores/connection';
  import { draftMessage } from '$lib/stores/draftMessage';
  import ConversationView from './ConversationView.svelte';

  let { sessionId }: { sessionId: string } = $props();

  let session = $state<Session | null>(null);
  let messages = $state<Message[]>([]);
  let native = $state<NativeSessionInfo | null>(null);
  let agentActive = $state(false);
  let sendPending = $state(false);
  const isLoading = $derived(sendPending || agentActive);
  const guardSend = createSendGuard((pending) => {
    sendPending = pending;
  });
  // The last poll failed. Cleared by the next successful one; the messages already shown stay.
  let loadError = $state<string | null>(null);
  // The last send was not delivered. Stays until dismissed or the next send.
  let sendError = $state<string | null>(null);

  const offline = $derived($connection.status === 'offline');
  // A cancelled or failed session takes no more messages (the backend refuses them).
  const notices = $derived(deliveryNotices(native?.deliveries ?? []));
  const ended = $derived(session?.status === 'cancelled' || session?.status === 'failed');

  onMount(() => {
    loadSession();
    // Native agent replies arrive from the kagent task after the POST returns: keep reading.
    const timer = setInterval(loadSession, 2500);
    return () => clearInterval(timer);
  });

  async function loadSession() {
    try {
      const result = await api.getSessionConversation(sessionId);
      session = result.session;
      messages = result.messages;
      // Delivery failures live in the ledger, not the conversation. A failed fetch keeps the last.
      native = await api.getSessionNative(sessionId).catch(() => native);
      agentActive = session.status === 'active';
      loadError = null;
    } catch (e) {
      console.error('Failed to load session:', e);
      loadError = "Couldn't refresh this session. Retrying…";
    }
  }

  async function handleSendMessage(detail: { message: string }) {
    await guardSend(() => sendMessage(detail.message));
  }

  async function handleRetry(message: Message) {
    await guardSend(async () => {
      try {
        await retryMessage(
          message,
          async () => {
            const [conversation, freshNative] = await Promise.all([
              api.getSessionConversation(sessionId),
              api.getSessionNative(sessionId)
            ]);
            session = conversation.session;
            native = freshNative;
            agentActive = session.status === 'active';
            return {
              conversationId: session.conversation_id,
              deliveries: freshNative?.deliveries ?? [],
              blocked: offline || ended || !freshNative || !!freshNative.turn_in_flight
            };
          },
          sendMessage
        );
      } catch (error) {
        sendError = error instanceof Error ? error.message : 'Could not refresh delivery state.';
      }
    });
  }

  async function sendMessage(userMessage: string) {
    if (!session || offline || ended || agentActive) return;

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

    try {
      await api.sendSessionMessage(sessionId, userMessage);
      // Reload messages to get the full response
      await loadSession();
    } catch (e) {
      console.error('Failed to send message:', e);
      // Not delivered: take the optimistic bubble back, keep the text, and say so.
      messages = messages.filter((m) => m.id !== tempId);
      draftMessage.set(userMessage);
      const reason = e instanceof Error && e.message ? e.message : 'Could not send the message.';
      sendError = `${reason} Your message is back in the box.`;
    }
  }
</script>

<ConversationView
  {messages}
  {isLoading}
  onSendMessage={handleSendMessage}
  placeholder={offline
    ? 'Backend unreachable…'
    : ended
      ? `This session is ${session?.status}.`
      : 'Message this session...'}
  emptyStateTitle="$ session --start"
  emptyStateMessage="This session's conversation will appear here"
  showInlineSessions={false}
  context={session?.title ?? 'session'}
  error={sendError ?? loadError}
  inputDisabled={offline || ended}
  deliveryNotices={notices}
  onRetry={handleRetry}
  onDismissError={() => {
    sendError = null;
    loadError = null;
  }}
/>
