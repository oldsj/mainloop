<script lang="ts">
  import { inbox, inboxItems, isInboxOpen, unreadCount } from '$lib/stores/inbox';
  import InboxItem from './InboxItem.svelte';

  let { desktop = false, mobile = false }: { desktop?: boolean; mobile?: boolean } = $props();

  function handleClose() {
    inbox.close();
  }

  function handleMarkAllRead() {
    inbox.markAllRead();
  }

  function handleClickOutside(e: MouseEvent) {
    // Skip click outside handling for desktop/mobile modes
    if (desktop || mobile) return;

    // Only process if panel is open
    if (!$isInboxOpen) return;

    const target = e.target as HTMLElement;
    if (target.closest('.inbox-panel') || target.closest('[aria-label="Open inbox"]')) {
      return;
    }
    inbox.close();
  }
</script>

<svelte:window on:click={handleClickOutside} />

{#if desktop || mobile || $isInboxOpen}
  <div
    class="inbox-panel flex h-full flex-col bg-white"
    class:fixed={!desktop && !mobile}
    class:right-0={!desktop && !mobile}
    class:top-0={!desktop && !mobile}
    class:z-50={!desktop && !mobile}
    class:w-full={!desktop && !mobile}
    class:max-w-md={!desktop && !mobile}
    class:border-l={!desktop && !mobile}
    class:border-neutral-200={!desktop && !mobile}
    class:shadow-xl={!desktop && !mobile}
  >
    <header class="flex items-center justify-between border-b border-neutral-200 px-4 py-3">
      <div class="flex items-center gap-2">
        <h2 class="text-lg font-semibold text-neutral-900">Inbox</h2>
        {#if $unreadCount > 0}
          <span class="rounded-full bg-neutral-100 px-2 py-0.5 text-xs font-medium text-neutral-600">
            {$unreadCount} unread
          </span>
        {/if}
      </div>
      <div class="flex items-center gap-2">
        {#if $unreadCount > 0}
          <button
            onclick={handleMarkAllRead}
            class="text-sm text-neutral-600 hover:text-neutral-900"
          >
            Mark all read
          </button>
        {/if}
        {#if !desktop && !mobile}
          <button
            onclick={handleClose}
            class="rounded-lg p-1 transition-colors hover:bg-neutral-100"
            aria-label="Close inbox"
          >
            <svg
              xmlns="http://www.w3.org/2000/svg"
              fill="none"
              viewBox="0 0 24 24"
              stroke-width="1.5"
              stroke="currentColor"
              class="h-6 w-6 text-neutral-600"
            >
              <path stroke-linecap="round" stroke-linejoin="round" d="M6 18 18 6M6 6l12 12" />
            </svg>
          </button>
        {/if}
      </div>
    </header>

    <div class="flex-1 overflow-y-auto">
      {#if $inboxItems.length === 0}
        <div class="flex h-full flex-col items-center justify-center px-4 text-center">
          <svg
            xmlns="http://www.w3.org/2000/svg"
            fill="none"
            viewBox="0 0 24 24"
            stroke-width="1.5"
            stroke="currentColor"
            class="mb-4 h-12 w-12 text-neutral-300"
          >
            <path
              stroke-linecap="round"
              stroke-linejoin="round"
              d="M2.25 13.5h3.86a2.25 2.25 0 0 1 2.012 1.244l.256.512a2.25 2.25 0 0 0 2.013 1.244h3.218a2.25 2.25 0 0 0 2.013-1.244l.256-.512a2.25 2.25 0 0 1 2.013-1.244h3.859m-19.5.338V18a2.25 2.25 0 0 0 2.25 2.25h15A2.25 2.25 0 0 0 21.75 18v-4.162c0-.224-.034-.447-.1-.661L19.24 5.338a2.25 2.25 0 0 0-2.15-1.588H6.911a2.25 2.25 0 0 0-2.15 1.588L2.35 13.177a2.25 2.25 0 0 0-.1.661Z"
            />
          </svg>
          <p class="text-neutral-500">No pending items</p>
          <p class="mt-1 text-sm text-neutral-400">You're all caught up!</p>
        </div>
      {:else}
        <div class="divide-y divide-neutral-200">
          {#each $inboxItems as item (item.id)}
            <InboxItem {item} />
          {/each}
        </div>
      {/if}
    </div>
  </div>
{/if}
