<script lang="ts">
  import type { Message } from '$lib/api';
  import { marked } from 'marked';

  let { message }: { message: Message } = $props();
  let isUser = $derived(message.role === 'user');

  // Configure marked for terminal aesthetic
  marked.setOptions({
    breaks: true,
    gfm: true
  });

  let htmlContent = $derived(marked.parse(message.content) as string);
</script>

<div
  class="w-full border-l-2 px-3 py-2 md:px-4 {isUser
    ? 'border-term-accent-alt bg-transparent'
    : 'border-term-accent bg-term-bg-secondary'}"
>
  <!-- Mobile: stacked, Desktop: inline -->
  <div class="flex flex-col gap-1 md:flex-row md:items-start md:gap-3">
    <span
      class="shrink-0 text-xs md:text-sm {isUser ? 'text-term-accent-alt' : 'text-term-accent'}"
    >
      {isUser ? '$ ' : '> '}
      <span class="hidden md:inline">{isUser ? 'user@mainloop' : 'claude@mainloop'}</span>
    </span>
    <div class="min-w-0 flex-1">
      <div class="prose-terminal text-sm text-term-fg md:text-base">
        {@html htmlContent}
      </div>
      <time class="mt-1 block text-xs text-term-fg-muted">
        {new Date(message.created_at).toLocaleTimeString()}
      </time>
    </div>
  </div>
</div>

<style>
  /* Terminal-styled markdown */
  .prose-terminal :global(p) {
    margin: 0 0 0.5em 0;
  }
  .prose-terminal :global(p:last-child) {
    margin-bottom: 0;
  }
  .prose-terminal :global(code) {
    background: var(--term-bg);
    border: 1px solid var(--term-border);
    padding: 0.125em 0.375em;
    font-size: 0.9em;
  }
  .prose-terminal :global(pre) {
    background: var(--term-bg);
    border: 1px solid var(--term-border);
    padding: 0.75em;
    overflow-x: auto;
    margin: 0.5em 0;
  }
  .prose-terminal :global(pre code) {
    background: none;
    border: none;
    padding: 0;
  }
  .prose-terminal :global(ul),
  .prose-terminal :global(ol) {
    margin: 0.5em 0;
    padding-left: 1.5em;
  }
  .prose-terminal :global(li) {
    margin: 0.25em 0;
  }
  .prose-terminal :global(ul) {
    list-style-type: disc;
  }
  .prose-terminal :global(ol) {
    list-style-type: decimal;
  }
  .prose-terminal :global(strong) {
    color: var(--term-accent);
    font-weight: 600;
  }
  .prose-terminal :global(em) {
    color: var(--term-fg-muted);
    font-style: italic;
  }
  .prose-terminal :global(a) {
    color: var(--term-info);
    text-decoration: underline;
  }
  .prose-terminal :global(a:hover) {
    color: var(--term-accent);
  }
  .prose-terminal :global(blockquote) {
    border-left: 2px solid var(--term-border);
    padding-left: 1em;
    margin: 0.5em 0;
    color: var(--term-fg-muted);
  }
  .prose-terminal :global(h1),
  .prose-terminal :global(h2),
  .prose-terminal :global(h3),
  .prose-terminal :global(h4) {
    color: var(--term-accent);
    margin: 0.75em 0 0.5em 0;
    font-weight: 600;
  }
  .prose-terminal :global(h1) {
    font-size: 1.25em;
  }
  .prose-terminal :global(h2) {
    font-size: 1.125em;
  }
  .prose-terminal :global(h3),
  .prose-terminal :global(h4) {
    font-size: 1em;
  }
  .prose-terminal :global(hr) {
    border: none;
    border-top: 1px solid var(--term-border);
    margin: 1em 0;
  }
  .prose-terminal :global(table) {
    border-collapse: collapse;
    margin: 0.5em 0;
    width: 100%;
  }
  .prose-terminal :global(th),
  .prose-terminal :global(td) {
    border: 1px solid var(--term-border);
    padding: 0.375em 0.75em;
    text-align: left;
  }
  .prose-terminal :global(th) {
    background: var(--term-bg);
    color: var(--term-accent);
  }
</style>
