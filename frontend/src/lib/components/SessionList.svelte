<script lang="ts">
  import { onMount } from 'svelte';
  import type { Session } from '$lib/api';
  import { sessions, activeSessions } from '$lib/stores/sessions';
  import SessionListItem from './SessionListItem.svelte';
  import SessionExpandedView from './SessionExpandedView.svelte';

  let expandedSessionId = $state<string | null>(null);

  onMount(() => {
    sessions.fetchSessions();
  });

  function handleSessionClick(session: Session) {
    if (expandedSessionId === session.id) {
      expandedSessionId = null;
    } else {
      expandedSessionId = session.id;
    }
  }

  function handleCloseExpanded() {
    expandedSessionId = null;
  }

  let expandedSession = $derived(
    $sessions.sessions.find((s) => s.id === expandedSessionId) || null
  );
</script>

<div class="flex h-full flex-col bg-term-bg">
  <!-- Header -->
  <div class="flex items-center justify-between border-b border-term-border p-3">
    <h2 class="text-sm font-medium text-term-fg">
      Sessions
      {#if $activeSessions.length > 0}
        <span class="ml-1 text-term-accent">({$activeSessions.length} active)</span>
      {/if}
    </h2>
    <button
      type="button"
      onclick={() => sessions.fetchSessions()}
      class="text-xs text-term-fg-muted hover:text-term-accent"
      aria-label="Refresh sessions"
    >
      <svg xmlns="http://www.w3.org/2000/svg" class="h-4 w-4" viewBox="0 0 20 20" fill="currentColor">
        <path fill-rule="evenodd" d="M4 2a1 1 0 011 1v2.101a7.002 7.002 0 0111.601 2.566 1 1 0 11-1.885.666A5.002 5.002 0 005.999 7H9a1 1 0 010 2H4a1 1 0 01-1-1V3a1 1 0 011-1zm.008 9.057a1 1 0 011.276.61A5.002 5.002 0 0014.001 13H11a1 1 0 110-2h5a1 1 0 011 1v5a1 1 0 11-2 0v-2.101a7.002 7.002 0 01-11.601-2.566 1 1 0 01.61-1.276z" clip-rule="evenodd" />
      </svg>
    </button>
  </div>

  <!-- Content -->
  <div class="flex min-h-0 flex-1">
    <!-- Session list -->
    <div class="w-full overflow-y-auto {expandedSession ? 'hidden md:block md:w-1/3' : ''}">
      {#if $sessions.loading && $sessions.sessions.length === 0}
        <div class="flex items-center justify-center p-8 text-term-fg-muted">
          <span>Loading sessions...</span>
        </div>
      {:else if $sessions.sessions.length === 0}
        <div class="flex flex-col items-center justify-center p-8 text-term-fg-muted">
          <p class="text-term-accent">$ sessions --list</p>
          <p class="mt-2 text-sm">No sessions yet</p>
          <p class="text-xs">Sessions appear when Claude spawns background work</p>
        </div>
      {:else}
        <div class="space-y-2 p-3">
          {#each $sessions.sessions as session (session.id)}
            <SessionListItem
              {session}
              isExpanded={expandedSessionId === session.id}
              onclick={() => handleSessionClick(session)}
            />
          {/each}
        </div>
      {/if}
    </div>

    <!-- Expanded view -->
    {#if expandedSession}
      <div class="h-full w-full border-l border-term-border md:w-2/3">
        <SessionExpandedView session={expandedSession} onClose={handleCloseExpanded} />
      </div>
    {/if}
  </div>
</div>
