<script lang="ts">
  import { page } from '$app/stores';
  import { inbox, inboxItems, unreadCount } from '$lib/stores/inbox';
  import { tasks } from '$lib/stores/tasks';
  import { api, type ActivityItem } from '$lib/api';
  import { visiblePolling, singleFlight } from '../visiblePolling';
  import HITLCard from './HITLCard.svelte';
  import QueueAttentionCard from './QueueAttentionCard.svelte';

  let { desktop = false, mobile = false }: { desktop?: boolean; mobile?: boolean } = $props();
  let view = $state<'attention' | 'activity'>('attention');
  let activity = $state<ActivityItem[]>([]);
  let activityLoaded = $state(false);
  let activityError = $state<string | null>(null);

  const refreshActivity = singleFlight(async (signal?: AbortSignal) => {
    try {
      const result = await api.listActivity(signal);
      if (signal?.aborted) return;
      activity = result;
      activityLoaded = true;
      activityError = null;
    } catch {
      if (!signal?.aborted) activityError = 'Could not load activity. Try again.';
    }
  });

  $effect(() => {
    if (!desktop && !mobile) return;
    void tasks.fetchList({});
    if (view === 'activity')
      return visiblePolling().watch('activity', (signal) => refreshActivity(signal));
  });

  $effect(() => {
    if ($page.url.searchParams.has('inbox')) {
      view = 'attention';
      inbox.open();
    }
  });
</script>

{#if desktop || mobile}
  <div class="bg-term-bg flex min-h-0 flex-col {mobile ? 'h-full' : ''}">
    <header class="border-term-border flex flex-wrap items-center gap-4 border-b px-4">
      <button
        type="button"
        class="focus-visible:outline-term-accent min-h-11 text-sm focus-visible:outline {view ===
        'attention'
          ? 'text-term-fg'
          : 'text-term-fg-muted hover:text-term-fg'}"
        aria-pressed={view === 'attention'}
        onclick={() => (view = 'attention')}
      >
        Needs you{#if $unreadCount > 0}<span class="text-term-info ml-2">{$unreadCount}</span>{/if}
      </button>
      <button
        type="button"
        class="focus-visible:outline-term-accent min-h-11 text-sm focus-visible:outline {view ===
        'activity'
          ? 'text-term-fg'
          : 'text-term-fg-muted hover:text-term-fg'}"
        aria-pressed={view === 'activity'}
        onclick={() => (view = 'activity')}>Activity</button
      >
    </header>
    <div class="min-h-0 flex-1 overflow-y-auto">
      {#if view === 'attention'}
        {#if $inbox.error}
          <div class="border-term-border border-b p-4" role="alert">
            <p class="text-term-red">Could not check what needs you.</p>
            <button
              type="button"
              class="text-term-accent min-h-11 text-sm hover:underline"
              onclick={() => inbox.fetchItems()}>Try again</button
            >
          </div>
        {/if}
        {#if !$inbox.error && !$inbox.loaded}
          <p class="text-term-fg-muted p-4 text-sm">Checking what needs you…</p>
        {:else if !$inbox.error && $inboxItems.length === 0}
          <div class="px-4 py-8">
            <p class="text-term-fg">Nothing needs you</p>
            <p class="text-term-fg-muted mt-2 text-sm">
              Approvals, questions and decisions appear here.
            </p>
          </div>
        {/if}
        {#each $inboxItems as entry (entry.id)}
          <div
            id="attention-{entry.queue_item?.hitl_request_id ?? entry.queue_item?.id ?? entry.id}"
            class="border-term-border border-b"
          >
            {#if entry.queue_item?.item_type === 'hitl_request' && entry.queue_item.hitl_request_id}
              <p class="text-term-fg px-4 pt-4 text-sm">{entry.needed}</p>
              <HITLCard requestId={entry.queue_item.hitl_request_id} />
            {:else if entry.queue_item}
              <QueueAttentionCard item={entry.queue_item} />
            {:else}
              <div class="p-4">
                <h3 class="text-term-fg text-sm">{entry.title}</h3>
                <p class="text-term-fg-muted mt-2 text-sm break-words">{entry.needed}</p>
                <a
                  href={entry.href}
                  class="text-term-accent focus-visible:outline-term-accent mt-2 inline-flex min-h-11 items-center text-sm underline underline-offset-4 focus-visible:outline"
                  >Review task and decide</a
                >
              </div>
            {/if}
          </div>
        {/each}
      {:else}
        {#if activityError}
          <div class="p-4" role="alert">
            <p class="text-term-red text-sm">{activityError}</p>
            <button
              type="button"
              class="text-term-accent min-h-11 hover:underline"
              onclick={() => refreshActivity()}>Try again</button
            >
          </div>
        {:else if !activityLoaded}
          <p class="text-term-fg-muted p-4 text-sm">Loading activity…</p>
        {:else if activity.length === 0}
          <p class="text-term-fg-muted p-4 text-sm">No activity yet</p>
        {/if}
        {#each activity as item (item.id)}
          <div class="border-term-border border-b px-4 py-3">
            <p class="text-term-fg-muted text-sm">{item.title}</p>
            <p class="text-term-fg-muted mt-1 text-xs break-words whitespace-pre-wrap">
              {item.content}
            </p>
            {#if item.href}<a
                href={item.href}
                class="text-term-accent mt-1 inline-flex min-h-11 items-center text-xs hover:underline"
                >View task</a
              >{/if}
            <time datetime={item.created_at} class="text-term-fg-muted block text-xs"
              >{new Date(item.created_at).toLocaleString()}</time
            >
          </div>
        {/each}
      {/if}
    </div>
  </div>
{/if}
