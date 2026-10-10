<script lang="ts">
  import { isCurrent, isFinished } from '$lib/sessionStatus';
  import { goto } from '$app/navigation';
  import { page } from '$app/stores';
  import type { Session } from '$lib/api';
  import { sessions, activeSessions, finishedSessions } from '$lib/stores/sessions';
  import { inbox } from '$lib/stores/inbox';
  import SessionListItem from './SessionListItem.svelte';

  function handleSessionClick(session: Session) {
    goto(`/sessions/${session.id}`);
  }

  let showFinished = $state(false);
  const loadError = $derived($inbox.error ?? $sessions.error);
  let showIdle = $state(false);
  const idleSessions = $derived($sessions.sessions.filter((s) => !isCurrent(s) && !isFinished(s)));
  const visibleSessions = $derived(
    $sessions.sessions.filter(
      (s) => isCurrent(s) || (showFinished && isFinished(s)) || (showIdle && !isFinished(s))
    )
  );
</script>

<div class="bg-term-bg flex h-full flex-col">
  <!-- Header -->
  <div class="border-term-border flex items-center justify-between border-b p-3">
    <a
      href="/agents"
      class="border-term-accent text-term-accent hover:bg-term-accent/10 mr-2 border px-2 py-0.5 text-xs"
      data-testid="new-agent-link">+ agent</a
    >
    <h2 class="text-term-fg text-sm font-medium">
      Sessions
      {#if $activeSessions.length > 0}
        <span
          class="bg-term-cyan/20 text-term-cyan ml-2 inline-flex items-center gap-1 rounded px-2 py-0.5 text-xs font-medium"
        >
          <span class="bg-term-cyan h-1.5 w-1.5 animate-pulse rounded-full"></span>
          {$activeSessions.length} current
        </span>
      {/if}
    </h2>
    <div class="flex items-center gap-2">
      <button
        type="button"
        onclick={() => inbox.fetchItems()}
        class="text-term-fg-muted hover:text-term-accent text-xs"
        aria-label="Refresh sessions"
      >
        <svg
          xmlns="http://www.w3.org/2000/svg"
          class="h-4 w-4"
          viewBox="0 0 20 20"
          fill="currentColor"
        >
          <path
            fill-rule="evenodd"
            d="M4 2a1 1 0 011 1v2.101a7.002 7.002 0 0111.601 2.566 1 1 0 11-1.885.666A5.002 5.002 0 005.999 7H9a1 1 0 010 2H4a1 1 0 01-1-1V3a1 1 0 011-1zm.008 9.057a1 1 0 011.276.61A5.002 5.002 0 0014.001 13H11a1 1 0 110-2h5a1 1 0 011 1v5a1 1 0 11-2 0v-2.101a7.002 7.002 0 01-11.601-2.566 1 1 0 01.61-1.276z"
            clip-rule="evenodd"
          />
        </svg>
      </button>
    </div>
  </div>

  {#if $finishedSessions.length > 0 || idleSessions.length > 0}
    <div class="border-term-border flex flex-wrap gap-x-4 border-b px-3">
      {#if $finishedSessions.length > 0}
        <button
          type="button"
          class="text-term-fg-muted hover:text-term-accent focus-visible:outline-term-accent min-h-11 text-xs focus-visible:outline"
          aria-pressed={showFinished}
          onclick={() => (showFinished = !showFinished)}
          data-testid="show-finished"
        >
          {showFinished ? 'Hide finished' : 'Show finished'} ({$finishedSessions.length})
        </button>
      {/if}
      {#if idleSessions.length > 0}
        <button
          type="button"
          class="text-term-fg-muted hover:text-term-accent focus-visible:outline-term-accent min-h-11 text-xs focus-visible:outline"
          aria-pressed={showIdle}
          onclick={() => (showIdle = !showIdle)}
        >
          {showIdle ? 'Hide idle' : 'Show idle'} ({idleSessions.length})
        </button>
      {/if}
    </div>
  {/if}

  <!-- Content -->
  <div class="min-h-0 flex-1 overflow-y-auto">
    {#if ($sessions.loading || (!$inbox.loaded && $inbox.isLoading)) && $sessions.sessions.length === 0}
      <div class="text-term-fg-muted flex items-center justify-center p-8">
        <span>Loading sessions...</span>
      </div>
    {:else if loadError && $sessions.sessions.length === 0}
      <div
        class="text-term-fg-muted flex flex-col items-center justify-center p-8"
        data-testid="sessions-error"
      >
        <p class="text-term-red">Couldn't load sessions</p>
        <p class="mt-2 text-xs">{loadError}</p>
        <button
          type="button"
          class="text-term-accent mt-3 text-xs hover:underline"
          onclick={() => inbox.fetchItems()}
        >
          retry
        </button>
      </div>
    {:else if visibleSessions.length === 0}
      <div class="text-term-fg-muted flex flex-col items-center justify-center p-8">
        <p class="mt-2 text-sm">No active sessions</p>
        <p class="text-xs">Start an agent with + agent, or delegate work from the home thread</p>
      </div>
    {:else}
      <div class="space-y-2 p-3">
        {#each visibleSessions as session (session.id)}
          <SessionListItem
            {session}
            selected={$page.params.id === session.id}
            onclick={() => handleSessionClick(session)}
          />
        {/each}
      </div>
    {/if}
  </div>
</div>
