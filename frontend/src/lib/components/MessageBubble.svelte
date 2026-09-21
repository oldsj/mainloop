<script lang="ts">
  import type { Message } from '$lib/api';
  import { renderMarkdown } from '$lib/markdown';
  import { parseChildReport } from '$lib/messages';
  import { messageTime } from '$lib/time';

  let { message, context = 'main' }: { message: Message; context?: string } = $props();
  let isUser = $derived(message.role === 'user');

  // A child agent's report, delivered to the main thread as a message: not something the user said.
  let report = $derived(isUser ? parseChildReport(message.content) : null);
  let htmlContent = $derived(renderMarkdown(report ? report.body : message.content));
</script>

<div
  class="message w-full border-l-2 px-3 py-2 md:px-4 {report
    ? 'border-term-fg-muted bg-term-bg-secondary/40'
    : 'border-term-border'} {!report && !isUser ? 'bg-term-bg-secondary' : 'bg-transparent'}"
  data-testid={report ? 'child-report' : undefined}
>
  <div class="flex flex-col gap-1">
    <span
      class="shrink-0 text-xs md:text-sm {report
        ? 'text-term-fg-muted'
        : isUser
          ? 'text-term-accent-alt'
          : 'text-term-accent'}"
    >
      {#if report}
        child · {report.title}{report.fallback ? ' (ended without a report)' : ''}
      {:else}
        {isUser ? 'user' : 'claude'}@{context}$
      {/if}
    </span>
    <div class="min-w-0 flex-1">
      <div class="prose-terminal text-term-fg text-sm md:text-base">
        {@html htmlContent}
      </div>
      <time class="text-term-fg-muted mt-1 block text-xs">
        {messageTime(message.created_at)}
      </time>
    </div>
  </div>
</div>

<style>
  /* Terminal-styled markdown */
  .prose-terminal {
    /* A long unbroken token (a path, a URL) wraps instead of running off the screen. */
    overflow-wrap: anywhere;
  }
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
    word-break: break-word;
  }
  .prose-terminal :global(pre) {
    background: var(--term-bg);
    border: 1px solid var(--term-border);
    padding: 0.75em;
    margin: 0.5em 0;
    overflow-x: hidden;
    white-space: pre-wrap;
    word-break: break-word;
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
    /* Wide tables scroll inside the bubble rather than stretching the page. */
    display: block;
    max-width: 100%;
    overflow-x: auto;
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
