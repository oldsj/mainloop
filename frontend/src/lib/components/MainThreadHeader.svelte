<script lang="ts">
  import type { MainThreadInfo } from '$lib/api';
  import { connection } from '$lib/stores/connection';
  import NativeIdentityStrip from './NativeIdentityStrip.svelte';

  let { info }: { info: MainThreadInfo } = $props();

  let open = $state(false);

  const native = $derived(info.native);
  const model = $derived((native?.model ?? 'claude').replace(/^claude-/, ''));
  const pending = $derived(info.topics.reduce((n, t) => n + t.pending, 0));
  // The last state we fetched is stale once the backend is unreachable; don't show it as live.
  const status = $derived(
    $connection.status === 'offline'
      ? 'unreachable'
      : native?.rotating
        ? 'rotating'
        : native?.turn_in_flight
          ? 'working'
          : native?.agent_live === false
            ? 'idle'
            : 'ready'
  );
  const dot = $derived(
    status === 'ready'
      ? 'bg-term-green'
      : status === 'idle'
        ? 'bg-term-fg-muted'
        : status === 'unreachable'
          ? 'bg-term-red'
          : 'bg-term-yellow animate-pulse'
  );
</script>

<div
  class="border-term-border bg-term-bg-secondary/40 text-term-fg-muted shrink-0 border-b text-xs"
  data-testid="main-thread-header"
>
  <div class="flex items-center gap-2 px-4 py-1.5">
    <span class="h-2 w-2 shrink-0 rounded-full {dot}" aria-hidden="true"></span>
    <span class="text-term-fg" data-testid="mt-state">{status}</span>
    <span>·</span>
    <span data-testid="mt-model">{model}</span>
    {#if pending > 0}
      <span>·</span>
      <span data-testid="mt-pending">{pending} pending</span>
    {/if}
    <button
      type="button"
      class="hover:text-term-accent ml-auto"
      aria-expanded={open}
      aria-controls="main-thread-details"
      onclick={() => (open = !open)}
      data-testid="mt-details-toggle"
    >
      details {open ? '▴' : '▾'}
    </button>
  </div>

  {#if open}
    <div id="main-thread-details" class="border-term-border border-t">
      {#if info.session_id}
        <NativeIdentityStrip sessionId={info.session_id} />
      {/if}
      <div class="px-4 py-1 font-mono" data-testid="topic-index">
        topics:
        {#each info.topics as t (t.name)}
          <span class="mr-3" data-testid="topic-line"
            >{t.name}{t.status_line ? ` (${t.status_line})` : ''} [{t.pending} pending]</span
          >
        {:else}
          <span>none yet</span>
        {/each}
      </div>
    </div>
  {/if}
</div>
