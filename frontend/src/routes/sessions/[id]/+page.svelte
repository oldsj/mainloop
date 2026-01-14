<script lang="ts">
  import { page } from '$app/stores';
  import { onMount } from 'svelte';
  import { api, type Session, type Message } from '$lib/api';
  import { sessions } from '$lib/stores/sessions';
  import SessionChat from '$lib/components/SessionChat.svelte';

  let sessionId = $derived($page.params.id);
  let session = $state<Session | null>(null);
  let logs = $state<string>('');
  let error = $state<string | null>(null);
  let activeTab = $state<'chat' | 'logs'>('chat');

  onMount(async () => {
    await loadSession();
  });

  async function loadSession() {
    if (!sessionId) {
      error = 'Session ID not provided';
      return;
    }
    try {
      session = await api.getSession(sessionId);
    } catch (e) {
      console.error('Failed to load session:', e);
      error = 'Session not found';
    }
  }

  async function handleCancel() {
    if (!session) return;
    if (confirm('Cancel this session?')) {
      await sessions.cancelSession(session.id);
      await loadSession();
    }
  }
</script>

<svelte:head>
  <title>{session?.title || 'Session'} - mainloop</title>
</svelte:head>

{#if error}
  <div class="flex h-full items-center justify-center text-term-red">
    <div class="text-center">
      <p class="text-lg">{error}</p>
      <a href="/" class="mt-4 inline-block text-term-accent hover:underline">Back to home</a>
    </div>
  </div>
{:else if !session}
  <div class="flex h-full items-center justify-center text-term-fg-muted">
    <span>Loading session...</span>
  </div>
{:else}
  <div class="flex h-full flex-col bg-term-bg">
    <!-- Header -->
    <div class="flex items-center justify-between border-b border-term-border p-4">
      <div class="min-w-0 flex-1">
        <div class="flex items-center gap-3">
          <a href="/" class="text-term-fg-muted hover:text-term-accent" aria-label="Back">
            <svg xmlns="http://www.w3.org/2000/svg" class="h-5 w-5" viewBox="0 0 20 20" fill="currentColor">
              <path fill-rule="evenodd" d="M9.707 16.707a1 1 0 01-1.414 0l-6-6a1 1 0 010-1.414l6-6a1 1 0 011.414 1.414L5.414 9H17a1 1 0 110 2H5.414l4.293 4.293a1 1 0 010 1.414z" clip-rule="evenodd" />
            </svg>
          </a>
          <div>
            <h1 class="text-lg font-medium text-term-fg">{session.title}</h1>
            <p class="text-sm text-term-fg-muted">{session.description}</p>
          </div>
        </div>
      </div>
      <div class="flex items-center gap-2">
        <span class="text-xs {session.status === 'active'
          ? 'text-term-cyan'
          : session.status === 'waiting_on_user'
            ? 'text-term-magenta'
            : session.status === 'completed'
              ? 'text-term-green'
              : session.status === 'failed'
                ? 'text-term-red'
                : 'text-term-yellow'}">
          [{session.status.toUpperCase().replace('_', ' ')}]
        </span>
        {#if session.status === 'active' || session.status === 'pending' || session.status === 'waiting_on_user'}
          <button
            type="button"
            onclick={handleCancel}
            class="border border-term-border px-3 py-1 text-sm text-term-fg-muted hover:border-term-red hover:text-term-red"
          >
            Cancel
          </button>
        {/if}
      </div>
    </div>

    <!-- Tabs -->
    <div class="flex border-b border-term-border">
      <button
        type="button"
        onclick={() => (activeTab = 'chat')}
        class="px-4 py-2 text-sm {activeTab === 'chat'
          ? 'border-b-2 border-term-accent text-term-accent'
          : 'text-term-fg-muted hover:text-term-fg'}"
      >
        Chat
      </button>
      <button
        type="button"
        onclick={() => (activeTab = 'logs')}
        class="px-4 py-2 text-sm {activeTab === 'logs'
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
          {#if logs}
            <pre class="whitespace-pre-wrap font-mono text-xs text-term-fg">{logs}</pre>
          {:else}
            <div class="flex h-full items-center justify-center text-term-fg-muted">
              <p>No logs available yet</p>
            </div>
          {/if}
        </div>
      {/if}
    </div>

    <!-- Summary (if completed) -->
    {#if session.status === 'completed' && session.summary}
      <div class="border-t border-term-border p-4">
        <h3 class="text-sm font-medium text-term-green">Summary</h3>
        <p class="mt-2 text-sm text-term-fg">{session.summary}</p>
      </div>
    {/if}

    <!-- Error (if failed) -->
    {#if session.status === 'failed' && session.error}
      <div class="border-t border-term-border p-4">
        <h3 class="text-sm font-medium text-term-red">Error</h3>
        <p class="mt-2 text-sm text-term-fg-muted">{session.error}</p>
      </div>
    {/if}
  </div>
{/if}
