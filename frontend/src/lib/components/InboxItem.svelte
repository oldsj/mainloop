<script lang="ts">
  import type { QueueItem } from '$lib/api';
  import { inbox } from '$lib/stores/inbox';

  let { item }: { item: QueueItem } = $props();
  let isResponding = $state(false);
  let customResponse = $state('');

  const priorityStyles: Record<string, string> = {
    urgent: 'border-l-term-error bg-term-bg-secondary',
    high: 'border-l-term-warning bg-term-bg-secondary',
    normal: 'border-l-term-info bg-term-bg-secondary',
    low: 'border-l-term-fg-muted bg-term-bg-secondary'
  };

  const typeIcons: Record<string, string> = {
    plan_ready:
      'M9 12h3.75M9 15h3.75M9 18h3.75m3 .75H18a2.25 2.25 0 0 0 2.25-2.25V6.108c0-1.135-.845-2.098-1.976-2.192a48.424 48.424 0 0 0-1.123-.08m-5.801 0c-.065.21-.1.433-.1.664 0 .414.336.75.75.75h4.5a.75.75 0 0 0 .75-.75 2.25 2.25 0 0 0-.1-.664m-5.8 0A2.251 2.251 0 0 1 13.5 2.25H15c1.012 0 1.867.668 2.15 1.586m-5.8 0c-.376.023-.75.05-1.124.08C9.095 4.01 8.25 4.973 8.25 6.108V8.25m0 0H4.875c-.621 0-1.125.504-1.125 1.125v11.25c0 .621.504 1.125 1.125 1.125h9.75c.621 0 1.125-.504 1.125-1.125V9.375c0-.621-.504-1.125-1.125-1.125H8.25ZM6.75 12h.008v.008H6.75V12Zm0 3h.008v.008H6.75V15Zm0 3h.008v.008H6.75V18Z',
    code_ready: 'M17.25 6.75 22.5 12l-5.25 5.25m-10.5 0L1.5 12l5.25-5.25m7.5-3-4.5 16.5',
    question:
      'M9.879 7.519c1.171-1.025 3.071-1.025 4.242 0 1.172 1.025 1.172 2.687 0 3.712-.203.179-.43.326-.67.442-.745.361-1.45.999-1.45 1.827v.75M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Zm-9 5.25h.008v.008H12v-.008Z',
    error:
      'M12 9v3.75m-9.303 3.376c-.866 1.5.217 3.374 1.948 3.374h14.71c1.73 0 2.813-1.874 1.948-3.374L13.949 3.378c-.866-1.5-3.032-1.5-3.898 0L2.697 16.126ZM12 15.75h.007v.008H12v-.008Z',
    notification:
      'M14.857 17.082a23.848 23.848 0 0 0 5.454-1.31A8.967 8.967 0 0 1 18 9.75V9A6 6 0 0 0 6 9v.75a8.967 8.967 0 0 1-2.312 6.022c1.733.64 3.56 1.085 5.455 1.31m5.714 0a24.255 24.255 0 0 1-5.714 0m5.714 0a3 3 0 1 1-5.714 0',
    routing_suggestion:
      'M7.5 21 3 16.5m0 0L7.5 12M3 16.5h13.5m0-13.5L21 7.5m0 0L16.5 12M21 7.5H7.5',
    feedback_addressed:
      'M9 12.75 11.25 15 15 9.75M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z',
    review:
      'M2.036 12.322a1.012 1.012 0 0 1 0-.639C3.423 7.51 7.36 4.5 12 4.5c4.638 0 8.573 3.007 9.963 7.178.07.207.07.431 0 .639C20.577 16.49 16.64 19.5 12 19.5c-4.638 0-8.573-3.007-9.963-7.178Z M15 12a3 3 0 1 1-6 0 3 3 0 0 1 6 0Z'
  };

  const iconPath = $derived(typeIcons[item.item_type] || typeIcons.notification);
  const prUrl = $derived(item.context?.pr_url as string | undefined);

  async function handleOption(option: string) {
    isResponding = true;
    try {
      await inbox.respond(item.id, option);
    } catch (e) {
      console.error('Failed to respond:', e);
    } finally {
      isResponding = false;
    }
  }

  async function handleCustomSubmit() {
    if (!customResponse.trim()) return;
    isResponding = true;
    try {
      await inbox.respond(item.id, customResponse);
      customResponse = '';
    } catch (e) {
      console.error('Failed to respond:', e);
    } finally {
      isResponding = false;
    }
  }

  function formatTime(dateStr: string): string {
    const date = new Date(dateStr);
    const now = new Date();
    const diffMs = now.getTime() - date.getTime();
    const diffMins = Math.floor(diffMs / 60000);
    const diffHours = Math.floor(diffMs / 3600000);
    const diffDays = Math.floor(diffMs / 86400000);

    if (diffMins < 1) return 'now';
    if (diffMins < 60) return `${diffMins}m`;
    if (diffHours < 24) return `${diffHours}h`;
    if (diffDays < 7) return `${diffDays}d`;
    return date.toLocaleDateString();
  }
</script>

<div
  class="border-l-2 p-4 transition-colors {priorityStyles[item.priority]} {item.read_at
    ? 'opacity-75'
    : ''}"
>
  <div class="flex items-start gap-3">
    <svg
      xmlns="http://www.w3.org/2000/svg"
      fill="none"
      viewBox="0 0 24 24"
      stroke-width="1.5"
      stroke="currentColor"
      class="mt-0.5 h-5 w-5 shrink-0 text-term-fg-muted"
    >
      <path stroke-linecap="square" stroke-linejoin="miter" d={iconPath} />
    </svg>

    <div class="min-w-0 flex-1">
      <div class="flex items-center justify-between gap-2">
        <h3 class="text-term-fg">{item.title}</h3>
        <span class="shrink-0 text-xs text-term-fg-muted">{formatTime(item.created_at)}</span>
      </div>

      <p class="mt-1 text-sm text-term-fg-muted">{item.content}</p>

      {#if prUrl}
        <a
          href={prUrl}
          target="_blank"
          rel="noopener noreferrer"
          class="mt-2 inline-flex items-center gap-1 text-sm text-term-info hover:underline"
        >
          <svg
            xmlns="http://www.w3.org/2000/svg"
            fill="none"
            viewBox="0 0 24 24"
            stroke-width="1.5"
            stroke="currentColor"
            class="h-4 w-4"
          >
            <path
              stroke-linecap="square"
              stroke-linejoin="miter"
              d="M13.5 6H5.25A2.25 2.25 0 0 0 3 8.25v10.5A2.25 2.25 0 0 0 5.25 21h10.5A2.25 2.25 0 0 0 18 18.75V10.5m-10.5 6L21 3m0 0h-5.25M21 3v5.25"
            />
          </svg>
          View PR
        </a>
      {/if}

      {#if item.options && item.options.length > 0 && item.status === 'pending'}
        <div class="mt-3 flex flex-wrap gap-2">
          {#each item.options as option}
            <button
              onclick={() => handleOption(option)}
              disabled={isResponding}
              class="border border-term-border bg-term-bg px-3 py-1.5 text-sm text-term-fg transition-colors hover:border-term-accent hover:text-term-accent disabled:opacity-50"
            >
              {option}
            </button>
          {/each}
        </div>
      {/if}

      {#if item.item_type === 'question' && item.status === 'pending'}
        <form
          onsubmit={(e) => {
            e.preventDefault();
            handleCustomSubmit();
          }}
          class="mt-3"
        >
          <div class="flex gap-2">
            <input
              type="text"
              bind:value={customResponse}
              placeholder="Type your response..."
              disabled={isResponding}
              class="flex-1 border border-term-border bg-term-bg px-3 py-1.5 text-sm text-term-fg placeholder:text-term-fg-muted focus:border-term-accent focus:outline-none disabled:opacity-50"
            />
            <button
              type="submit"
              disabled={isResponding || !customResponse.trim()}
              class="border border-term-border bg-term-bg px-3 py-1.5 text-sm text-term-fg transition-colors hover:border-term-accent hover:text-term-accent disabled:opacity-50"
            >
              SEND
            </button>
          </div>
        </form>
      {/if}

      {#if item.status === 'responded' && item.response}
        <div class="mt-2 border border-term-border bg-term-bg px-2 py-1 text-sm text-term-fg-muted">
          > {item.response}
        </div>
      {/if}
    </div>
  </div>
</div>
