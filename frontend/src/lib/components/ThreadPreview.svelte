<script lang="ts">
  import type { Thread } from '$lib/types/thread';
  import { threadStatusConfig, getLatestThreadMessage, getReplyCount } from '$lib/types/thread';

  let {
    thread,
    expanded = false,
    onToggle
  }: {
    thread: Thread;
    expanded?: boolean;
    onToggle?: () => void;
  } = $props();

  let config = $derived(threadStatusConfig[thread.status]);
  let latestMessage = $derived(getLatestThreadMessage(thread));
  let replyCount = $derived(getReplyCount(thread));
  let isWaiting = $derived(thread.status === 'waiting');
</script>

<button
  type="button"
  onclick={onToggle}
  class="mt-2 flex w-full items-center gap-2 border border-term-border bg-term-bg px-3 py-2 text-left transition-colors {config.hoverBorderClass} {isWaiting
    ? 'border-term-warning'
    : ''}"
>
  <!-- Status icon -->
  <span
    class="shrink-0 text-sm {config.textClass} {thread.status === 'active'
      ? 'animate-pulse'
      : ''}"
  >
    {config.icon}
  </span>

  <!-- Thread title -->
  <span class="text-sm text-term-fg">
    {thread.title}
  </span>

  <!-- Latest message preview (truncated) -->
  {#if latestMessage && !expanded}
    <span class="flex-1 truncate text-xs text-term-fg-muted">
      — {latestMessage.content.slice(0, 50)}{latestMessage.content.length > 50 ? '...' : ''}
    </span>
  {/if}

  <!-- Reply count and unread badge -->
  <span class="flex shrink-0 items-center gap-2 text-xs">
    {#if thread.unreadCount > 0}
      <span
        class="flex h-5 min-w-5 items-center justify-center bg-term-warning px-1 text-term-bg"
      >
        {thread.unreadCount}
      </span>
    {/if}
    <span class="text-term-fg-muted">
      {replyCount} {replyCount === 1 ? 'reply' : 'replies'}
    </span>
    <span class="text-term-fg-muted">
      {expanded ? '▼' : '▶'}
    </span>
  </span>
</button>
