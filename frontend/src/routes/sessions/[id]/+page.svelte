<script lang="ts">
  import { page } from '$app/stores';
  import { get } from 'svelte/store';
  import { goto } from '$app/navigation';
  import { api, ApiError, type Session } from '$lib/api';
  import { sessions } from '$lib/stores/sessions';
  import { connection } from '$lib/stores/connection';
  import { statusLabel } from '$lib/sessionStatus';
  import SessionChat from '$lib/components/SessionChat.svelte';
  import NativeIdentityStrip from '$lib/components/NativeIdentityStrip.svelte';
  import WorkspaceLifecycleBadge from '$lib/components/WorkspaceLifecycleBadge.svelte';
  import { workspaces } from '$lib/stores/workspaces';
  import { tasks, taskViews } from '$lib/stores/tasks';
  import { statusLabel as taskStatusLabel, taskForSession } from '$lib/taskState';

  let sessionId = $derived($page.params.id);
  let loaded = $state<Session | null>(null);
  let error = $state<string | null>(null);
  // The backend couldn't be reached (as opposed to the session not existing): retried on recovery.
  let unreachable = $state(false);
  let stoppingId = $state<string | null>(null);

  // The sessions store is kept current by SSE; the page's own fetch only gets it started.
  const live = $derived($sessions.sessions.find((s) => s.id === sessionId));
  const session = $derived(loaded ? { ...loaded, ...live } : null);
  const workspace = $derived(
    $workspaces.workspaces.find((item) => item.session_id === session?.id)
  );

  const owningTask = $derived(session ? taskForSession($taskViews, session.id) : null);
  $effect(() => {
    void tasks.fetchList({});
  });

  // SvelteKit reuses this component when only [id] changes, so load per id rather than on mount.
  // Clearing `loaded` first unmounts the chat and identity strip, which read their id once.
  $effect(() => {
    const id = sessionId;
    loaded = null;
    error = null;
    unreachable = false;
    actionNotice = null;
    void loadSession(id);
  });

  // Retry a load that failed because the backend was down once it is back.
  let seenRecoveries = $connection.recoveries;
  $effect(() => {
    const recoveries = $connection.recoveries;
    if (recoveries === seenRecoveries) return;
    seenRecoveries = recoveries;
    if (unreachable && sessionId) {
      unreachable = false;
      error = null;
      void loadSession(sessionId);
    }
  });

  async function loadSession(id: string | undefined = sessionId) {
    if (!id) {
      error = 'Session ID not provided';
      return;
    }
    try {
      const result = await api.getSession(id);
      // A slow response for a session we've already left must not replace the current one.
      if (id === sessionId) loaded = result;
    } catch (e) {
      console.error('Failed to load session:', e);
      if (id !== sessionId) return;
      // An HTTP error is distinct from a failed connection, even if a health probe failed too.
      unreachable = !(e instanceof ApiError) &&
        (e instanceof TypeError || get(connection).status === 'offline');
      error = e instanceof ApiError
        ? e.status >= 500
          ? 'Mainloop returned a server error. Reload to try again.'
          : e.status === 404 ? 'Session not found' : 'Session could not be loaded.'
        : unreachable ? "Can't reach the Mainloop backend." : 'Session could not be loaded.';
    }
  }

  // The result of the last cancel/archive, shown under the header: the backend can refuse, and a
  // cancel can succeed without being able to confirm that the agent's process stopped.
  let actionNotice = $state<{ kind: 'error' | 'warning'; text: string } | null>(null);

  const finished = $derived(
    session?.status === 'completed' || session?.status === 'failed' || session?.status === 'cancelled'
  );

  async function handleStopTurn() {
    if (!session || stoppingId === session.id) return;
    const id = session.id;
    stoppingId = id;
    actionNotice = null;
    try {
      await api.stopTurn(id);
      if (id === sessionId) await loadSession(id);
    } catch (e) {
      if (id === sessionId) {
        actionNotice = {
          kind: 'error',
          text: e instanceof Error ? e.message : 'Could not confirm the turn stopped. Refresh before retrying.'
        };
      }
    } finally {
      if (stoppingId === id) stoppingId = null;
    }
  }

  async function handleCancel() {
    if (!session) return;
    if (!confirm('Cancel this session? Its agent is stopped.')) return;
    actionNotice = null;
    const result = await sessions.cancelSession(session.id);
    if (!result.ok) {
      actionNotice = { kind: 'error', text: result.message };
    } else if (result.unconfirmed) {
      actionNotice = {
        kind: 'warning',
        text: "Cancelled, but Mainloop couldn't confirm the agent stopped; it may still be running."
      };
    }
    await loadSession();
  }

  async function handleArchive() {
    if (!session) return;
    actionNotice = null;
    try {
      await sessions.archiveSession(session.id);
      await goto('/');
    } catch (e) {
      actionNotice = { kind: 'error', text: e instanceof Error ? e.message : 'Failed to clear' };
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
      {#if unreachable}
        <p class="mt-2 text-sm text-term-fg-muted">Retrying automatically…</p>
      {/if}
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
    <div class="flex flex-wrap items-center justify-between gap-3 border-b border-term-border p-4">
      <div class="min-w-0 flex-1">
        <div class="flex items-center gap-3">
          <a href="/" class="text-term-fg-muted hover:text-term-accent" aria-label="Back">
            <svg xmlns="http://www.w3.org/2000/svg" class="h-5 w-5" viewBox="0 0 20 20" fill="currentColor">
              <path fill-rule="evenodd" d="M9.707 16.707a1 1 0 01-1.414 0l-6-6a1 1 0 010-1.414l6-6a1 1 0 011.414 1.414L5.414 9H17a1 1 0 110 2H5.414l4.293 4.293a1 1 0 010 1.414z" clip-rule="evenodd" />
            </svg>
          </a>
          <div class="min-w-0">
            <h1 class="truncate text-lg font-medium text-term-fg">{session.title}</h1>
            <p class="truncate text-sm text-term-fg-muted">{session.description}</p>
          </div>
        </div>
      </div>
      <div class="flex shrink-0 items-center gap-2">
        <span class="text-xs {session.status === 'active'
          ? 'text-term-cyan'
          : session.status === 'waiting_on_user'
            ? 'text-term-magenta'
            : session.status === 'completed'
              ? 'text-term-green'
              : session.status === 'failed'
                ? 'text-term-red'
                : 'text-term-yellow'}" data-testid="session-status">
          [{statusLabel(session.status)}]
        </span>
        {#if !session.archived_at && session.status !== 'failed' && session.status !== 'cancelled'}
          <button
            type="button"
            onclick={handleStopTurn}
            disabled={stoppingId === session.id || $connection.status === 'offline'}
            class="min-h-11 border border-term-border px-3 py-1 text-sm text-term-fg hover:border-term-red hover:text-term-red disabled:opacity-50 focus-visible:outline focus-visible:outline-2 focus-visible:outline-term-accent"
            data-testid="session-stop-turn"
            title="Stop the current turn and keep this session available"
          >
            {stoppingId === session.id ? 'Stopping…' : 'Stop turn'}
          </button>
        {/if}
        {#if session.status === 'active' || session.status === 'pending' || session.status === 'waiting_on_user'}
          <button
            type="button"
            onclick={handleCancel}
            class="border border-term-border px-3 py-1 text-sm text-term-fg-muted hover:border-term-red hover:text-term-red"
          >
            Cancel
          </button>
        {:else if finished}
          <button
            type="button"
            onclick={handleArchive}
            class="border border-term-border px-3 py-1 text-sm text-term-fg-muted hover:border-term-accent hover:text-term-accent"
            data-testid="archive-session"
            title="Clear this session from the list (kept for audit)"
          >
            Clear
          </button>
        {/if}
      </div>
    </div>

    {#if actionNotice}
      <div
        class="border-b px-4 py-2 text-sm {actionNotice.kind === 'error'
          ? 'border-term-red/60 bg-term-red/10 text-term-red'
          : 'border-term-yellow/60 bg-term-yellow/10 text-term-yellow'}"
        role="alert"
        data-testid="session-action-notice"
      >
        {actionNotice.text}
      </div>
    {/if}

    <NativeIdentityStrip sessionId={session.id} />

    {#if owningTask}
      <div
        class="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-term-border px-4 py-2 text-sm"
        data-testid="session-task-link"
      >
        <span class="text-term-fg-muted">Task</span>
        <a
          href="/tasks/{owningTask.view.task.id}"
          class="text-term-accent underline underline-offset-4 hover:text-term-fg"
        >
          {owningTask.view.task.title}
        </a>
        <span class="text-term-fg-muted text-xs">
          {taskStatusLabel(owningTask.view.task.status)}{owningTask.current
            ? ''
            : ' · attempt superseded, history only'}
        </span>
      </div>
    {/if}

    {#if workspace}
      <div class="flex items-center gap-3 border-b border-term-border px-4 py-2 text-sm">
        <span class="text-term-fg-muted">Workspace</span>
        <WorkspaceLifecycleBadge {workspace} />
        <a
          href={`/workspaces/${workspace.workspace_id}`}
          class="ml-auto text-term-accent underline underline-offset-4 hover:text-term-fg"
        >
          Manage workspace
        </a>
      </div>
    {/if}

    <div class="min-h-0 flex-1">
      <SessionChat sessionId={session.id} />
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
