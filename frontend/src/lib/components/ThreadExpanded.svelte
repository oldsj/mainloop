<script lang="ts">
  import type { Thread } from '$lib/types/thread';
  import { threadStatusConfig } from '$lib/types/thread';
  import { marked } from 'marked';

  let {
    thread,
    onClose,
    onReply
  }: {
    thread: Thread;
    onClose?: () => void;
    onReply?: (message: string) => void;
  } = $props();

  let config = $derived(threadStatusConfig[thread.status]);
  let replyText = $state('');

  function handleSubmit(e: Event) {
    e.preventDefault();
    if (replyText.trim() && onReply) {
      onReply(replyText.trim());
      replyText = '';
    }
  }

  function handleKeyDown(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      handleSubmit(e);
    }
  }
</script>

<div class="mt-2 border border-term-border bg-term-bg">
  <!-- Header -->
  <div
    class="flex items-center justify-between border-b border-term-border px-3 py-2"
  >
    <div class="flex items-center gap-2">
      <span class="text-sm {config.textClass}">{config.icon}</span>
      <span class="text-sm font-medium text-term-fg">{thread.title}</span>
      <span class="text-xs {config.textClass}">{config.label}</span>
    </div>
    <button
      type="button"
      onclick={onClose}
      class="text-xs text-term-fg-muted hover:text-term-fg"
    >
      [close]
    </button>
  </div>

  <!-- Messages -->
  <div class="max-h-64 space-y-2 overflow-y-auto p-3">
    {#each thread.messages as msg (msg.id)}
      <div
        class="border-l-2 px-3 py-1 {msg.role === 'user'
          ? 'border-term-accent-alt'
          : 'border-term-accent'}"
      >
        <div class="flex items-center gap-2">
          <span
            class="text-xs {msg.role === 'user' ? 'text-term-accent-alt' : 'text-term-accent'}"
          >
            {msg.role === 'user' ? '$ you' : '> worker'}
          </span>
          <time class="text-xs text-term-fg-muted">
            {new Date(msg.timestamp).toLocaleTimeString()}
          </time>
        </div>
        <div class="prose-terminal mt-1 text-sm text-term-fg">
          {@html marked.parse(msg.content)}
        </div>
      </div>
    {/each}

    {#if thread.messages.length === 0}
      <p class="text-sm text-term-fg-muted">Starting...</p>
    {/if}
  </div>

  <!-- Result (PR link, etc) -->
  {#if thread.result?.prUrl}
    <div class="border-t border-term-border px-3 py-2">
      <a
        href={thread.result.prUrl}
        target="_blank"
        rel="noopener noreferrer"
        class="text-sm text-term-info hover:underline"
      >
        View PR #{thread.result.prNumber}
      </a>
    </div>
  {/if}

  <!-- Reply input -->
  {#if thread.status !== 'completed'}
    <form
      onsubmit={handleSubmit}
      class="flex gap-2 border-t border-term-border p-3"
    >
      <input
        type="text"
        bind:value={replyText}
        onkeydown={handleKeyDown}
        placeholder={thread.status === 'waiting' ? 'Reply to continue...' : 'Add a reply...'}
        class="flex-1 border border-term-border bg-term-bg px-2 py-1 text-sm text-term-fg placeholder:text-term-fg-muted focus:border-term-accent focus:outline-none"
      />
      <button
        type="submit"
        disabled={!replyText.trim()}
        class="border border-term-accent bg-term-bg px-3 py-1 text-sm text-term-accent hover:bg-term-accent hover:text-term-bg disabled:opacity-50"
      >
        Reply
      </button>
    </form>
  {/if}
</div>

<style>
  .prose-terminal :global(p) {
    margin: 0 0 0.5em 0;
  }
  .prose-terminal :global(p:last-child) {
    margin-bottom: 0;
  }
  .prose-terminal :global(strong) {
    color: var(--term-accent);
    font-weight: 600;
  }
  .prose-terminal :global(code) {
    background: var(--term-bg-secondary);
    padding: 0.125em 0.375em;
    font-size: 0.9em;
  }
  .prose-terminal :global(ol),
  .prose-terminal :global(ul) {
    margin: 0.5em 0;
    padding-left: 1.5em;
  }
  .prose-terminal :global(li) {
    margin: 0.25em 0;
  }
</style>
