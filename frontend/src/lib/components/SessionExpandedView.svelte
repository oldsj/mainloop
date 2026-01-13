<script lang="ts">
  import type { Session } from '$lib/api';
  import { sessions } from '$lib/stores/sessions';
  import SessionChat from './SessionChat.svelte';

  let {
    session,
    onClose
  }: {
    session: Session;
    onClose?: () => void;
  } = $props();

  let activeTab = $state<'chat' | 'logs'>('chat');

  async function handleCancel() {
    if (confirm('Cancel this session?')) {
      await sessions.cancelSession(session.id);
      onClose?.();
    }
  }
</script>

<div class="flex h-full flex-col border border-term-border bg-term-bg">
  <!-- Header -->
  <div class="flex items-center justify-between border-b border-term-border p-3">
    <div class="min-w-0 flex-1">
      <h2 class="truncate text-sm font-medium text-term-fg">{session.title}</h2>
      <p class="truncate text-xs text-term-fg-muted">{session.description}</p>
    </div>
    <div class="flex shrink-0 items-center gap-2">
      {#if session.status === 'active' || session.status === 'pending' || session.status === 'waiting_on_user'}
        <button
          type="button"
          onclick={handleCancel}
          class="border border-term-border px-2 py-1 text-xs text-term-fg-muted hover:border-term-red hover:text-term-red"
        >
          Cancel
        </button>
      {/if}
      <a
        href="/sessions/{session.id}"
        class="border border-term-border px-2 py-1 text-xs text-term-fg-muted hover:border-term-accent hover:text-term-accent"
      >
        Fullscreen
      </a>
      {#if onClose}
        <button
          type="button"
          onclick={onClose}
          class="text-term-fg-muted hover:text-term-accent"
          aria-label="Close"
        >
          <svg xmlns="http://www.w3.org/2000/svg" class="h-4 w-4" viewBox="0 0 20 20" fill="currentColor">
            <path fill-rule="evenodd" d="M4.293 4.293a1 1 0 011.414 0L10 8.586l4.293-4.293a1 1 0 111.414 1.414L11.414 10l4.293 4.293a1 1 0 01-1.414 1.414L10 11.414l-4.293 4.293a1 1 0 01-1.414-1.414L8.586 10 4.293 5.707a1 1 0 010-1.414z" clip-rule="evenodd" />
          </svg>
        </button>
      {/if}
    </div>
  </div>

  <!-- Tabs -->
  <div class="flex border-b border-term-border">
    <button
      type="button"
      onclick={() => (activeTab = 'chat')}
      class="px-4 py-2 text-xs {activeTab === 'chat'
        ? 'border-b-2 border-term-accent text-term-accent'
        : 'text-term-fg-muted hover:text-term-fg'}"
    >
      Chat
    </button>
    <button
      type="button"
      onclick={() => (activeTab = 'logs')}
      class="px-4 py-2 text-xs {activeTab === 'logs'
        ? 'border-b-2 border-term-accent text-term-accent'
        : 'text-term-fg-muted hover:text-term-fg'}"
    >
      Logs
    </button>
  </div>

  <!-- Content -->
  <div class="min-h-0 flex-1">
    {#if activeTab === 'chat'}
      <SessionChat sessionId={session.id} />
    {:else}
      <div class="h-full overflow-auto p-4">
        <pre class="font-mono text-xs text-term-fg-muted">Logs not yet implemented...</pre>
      </div>
    {/if}
  </div>
</div>
