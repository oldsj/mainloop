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
    if (desktop || mobile) return;
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
    class="inbox-panel flex h-full flex-col bg-term-bg"
    class:fixed={!desktop && !mobile}
    class:right-0={!desktop && !mobile}
    class:top-0={!desktop && !mobile}
    class:z-50={!desktop && !mobile}
    class:w-full={!desktop && !mobile}
    class:max-w-md={!desktop && !mobile}
    class:border-l={!desktop && !mobile}
    class:border-term-border={!desktop && !mobile}
    class:shadow-xl={!desktop && !mobile}
  >
    <header class="flex items-center justify-between border-b border-term-border px-4 py-3">
      <div class="flex items-center gap-2">
        <h2 class="text-term-fg">[INBOX]</h2>
        {#if $unreadCount > 0}
          <span class="border border-term-fg-muted px-2 py-0.5 text-xs text-term-fg-muted">
            {$unreadCount} unread
          </span>
        {/if}
      </div>
      <div class="flex items-center gap-2">
        {#if $unreadCount > 0}
          <button
            onclick={handleMarkAllRead}
            class="text-sm text-term-fg-muted hover:text-term-fg"
          >
            Mark all read
          </button>
        {/if}
        {#if !desktop && !mobile}
          <button
            onclick={handleClose}
            class="p-1 text-term-fg-muted transition-colors hover:text-term-fg"
            aria-label="Close inbox"
          >
            <svg
              xmlns="http://www.w3.org/2000/svg"
              fill="none"
              viewBox="0 0 24 24"
              stroke-width="1.5"
              stroke="currentColor"
              class="h-6 w-6"
            >
              <path stroke-linecap="square" stroke-linejoin="miter" d="M6 18 18 6M6 6l12 12" />
            </svg>
          </button>
        {/if}
      </div>
    </header>

    <div class="flex-1 overflow-y-auto">
      {#if $inboxItems.length === 0}
        <div class="flex h-full flex-col items-center justify-center px-4 text-center">
          <p class="text-term-fg-muted">$ cat inbox</p>
          <p class="mt-2 text-term-fg-muted">No pending items</p>
          <p class="mt-1 text-sm text-term-fg-muted">You're all caught up!</p>
        </div>
      {:else}
        <div class="divide-y divide-term-border">
          {#each $inboxItems as item (item.id)}
            <InboxItem {item} />
          {/each}
        </div>
      {/if}
    </div>
  </div>
{/if}
