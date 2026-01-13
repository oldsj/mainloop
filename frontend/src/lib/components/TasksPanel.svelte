<script lang="ts">
  import { inbox, inboxItems, unreadCount } from '$lib/stores/inbox';
  import type { QueueItem } from '$lib/api';

  let { desktop = false, mobile = false }: { desktop?: boolean; mobile?: boolean } = $props();

  let respondingItemId = $state<string | null>(null);
  let customResponses = $state<Record<string, string>>({});
  let expandedPlanId = $state<string | null>(null);

  function formatTime(dateStr: string): string {
    const date = new Date(dateStr);
    const now = new Date();
    const diff = now.getTime() - date.getTime();
    const minutes = Math.floor(diff / 60000);
    const hours = Math.floor(minutes / 60);
    const days = Math.floor(hours / 24);

    if (minutes < 1) return 'now';
    if (minutes < 60) return `${minutes}m`;
    if (hours < 24) return `${hours}h`;
    return `${days}d`;
  }

  // Priority styles
  const priorityStyles: Record<string, string> = {
    urgent: 'border-l-term-error',
    high: 'border-l-term-warning',
    normal: 'border-l-term-info',
    low: 'border-l-term-fg-muted'
  };

  // Type icons
  const typeIcons: Record<string, string> = {
    plan_review:
      'M19.5 14.25v-2.625a3.375 3.375 0 0 0-3.375-3.375h-1.5A1.125 1.125 0 0 1 13.5 7.125v-1.5a3.375 3.375 0 0 0-3.375-3.375H8.25m0 12.75h7.5m-7.5 3H12M10.5 2.25H5.625c-.621 0-1.125.504-1.125 1.125v17.25c0 .621.504 1.125 1.125 1.125h12.75c.621 0 1.125-.504 1.125-1.125V11.25a9 9 0 0 0-9-9Z',
    question:
      'M9.879 7.519c1.171-1.025 3.071-1.025 4.242 0 1.172 1.025 1.172 2.687 0 3.712-.203.179-.43.326-.67.442-.745.361-1.45.999-1.45 1.827v.75M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Zm-9 5.25h.008v.008H12v-.008Z',
    error:
      'M12 9v3.75m-9.303 3.376c-.866 1.5.217 3.374 1.948 3.374h14.71c1.73 0 2.813-1.874 1.948-3.374L13.949 3.378c-.866-1.5-3.032-1.5-3.898 0L2.697 16.126ZM12 15.75h.007v.008H12v-.008Z',
    notification:
      'M14.857 17.082a23.848 23.848 0 0 0 5.454-1.31A8.967 8.967 0 0 1 18 9.75V9A6 6 0 0 0 6 9v.75a8.967 8.967 0 0 1-2.312 6.022c1.733.64 3.56 1.085 5.455 1.31m5.714 0a24.255 24.255 0 0 1-5.714 0m5.714 0a3 3 0 1 1-5.714 0'
  };

  function getIconPath(itemType: string): string {
    return typeIcons[itemType] || typeIcons.notification;
  }

  function getPrUrl(item: QueueItem): string | undefined {
    return item.context?.pr_url as string | undefined;
  }

  async function handleInboxOption(itemId: string, option: string) {
    respondingItemId = itemId;
    try {
      await inbox.respond(itemId, option);
    } catch (e) {
      console.error('Failed to respond:', e);
    } finally {
      respondingItemId = null;
    }
  }

  async function handleCustomSubmit(itemId: string) {
    const response = customResponses[itemId]?.trim();
    if (!response) return;
    respondingItemId = itemId;
    try {
      await inbox.respond(itemId, response);
      customResponses[itemId] = '';
    } catch (e) {
      console.error('Failed to respond:', e);
    } finally {
      respondingItemId = null;
    }
  }
</script>

{#if desktop || mobile}
  <div class="flex h-full flex-col bg-term-bg">
    <header class="flex items-center justify-between border-b border-term-border px-4 py-3">
      <div class="flex items-center gap-2">
        <h2 class="text-term-fg">[INBOX]</h2>
        {#if $unreadCount > 0}
          <span class="border border-term-info px-2 py-0.5 text-xs text-term-info">
            {$unreadCount}
          </span>
        {/if}
      </div>
    </header>

    <div class="flex-1 overflow-y-auto">
      {#if $inboxItems.length === 0}
        <div class="flex h-full flex-col items-center justify-center px-4 text-center">
          <p class="text-term-fg-muted">$ ls inbox/</p>
          <p class="mt-2 text-term-fg-muted">All caught up</p>
        </div>
      {:else}
        <div>
          {#each $inboxItems as item (item.id)}
            <div
              class="border-b border-l-2 border-term-border p-4 transition-colors {priorityStyles[item.priority]} {item.read_at
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
                  <path stroke-linecap="square" stroke-linejoin="miter" d={getIconPath(item.item_type)} />
                </svg>

                <div class="min-w-0 flex-1">
                  <div class="flex items-center justify-between gap-2">
                    <h3 class="text-term-fg">{item.title}</h3>
                    <span class="shrink-0 text-xs text-term-fg-muted">{formatTime(item.created_at)}</span>
                  </div>

                  {#if item.item_type === 'plan_review'}
                    <button
                      type="button"
                      onclick={() => (expandedPlanId = expandedPlanId === item.id ? null : item.id)}
                      class="mt-2 flex w-full items-center gap-2 text-left text-sm text-term-info hover:underline"
                    >
                      <svg
                        xmlns="http://www.w3.org/2000/svg"
                        fill="none"
                        viewBox="0 0 24 24"
                        stroke-width="1.5"
                        stroke="currentColor"
                        class="h-4 w-4 transition-transform {expandedPlanId === item.id ? 'rotate-180' : ''}"
                      >
                        <path stroke-linecap="square" stroke-linejoin="miter" d="m19.5 8.25-7.5 7.5-7.5-7.5" />
                      </svg>
                      {expandedPlanId === item.id ? 'Hide plan' : 'View plan'}
                    </button>

                    {#if expandedPlanId === item.id}
                      <div class="mt-3 max-h-96 overflow-y-auto rounded border border-term-border bg-term-bg-secondary p-3">
                        <pre class="whitespace-pre-wrap text-xs text-term-fg">{item.content}</pre>
                      </div>
                    {/if}
                  {:else}
                    <p class="mt-1 text-sm text-term-fg-muted">{item.content}</p>
                  {/if}

                  {#if getPrUrl(item)}
                    <a
                      href={getPrUrl(item)}
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
                          type="button"
                          onclick={() => handleInboxOption(item.id, option)}
                          disabled={respondingItemId === item.id}
                          class="border border-term-border bg-term-bg px-3 py-1.5 text-sm text-term-fg transition-colors hover:border-term-accent hover:text-term-accent disabled:opacity-50"
                        >
                          {option}
                        </button>
                      {/each}
                    </div>
                  {/if}

                  {#if (item.item_type === 'question' || item.item_type === 'plan_review') && item.status === 'pending'}
                    <form
                      onsubmit={(e) => {
                        e.preventDefault();
                        handleCustomSubmit(item.id);
                      }}
                      class="mt-3"
                    >
                      <div class="flex gap-2">
                        <input
                          type="text"
                          bind:value={customResponses[item.id]}
                          placeholder={item.item_type === 'plan_review' ? 'Request changes...' : 'Type your response...'}
                          disabled={respondingItemId === item.id}
                          class="flex-1 border border-term-border bg-term-bg px-3 py-1.5 text-sm text-term-fg placeholder:text-term-fg-muted focus:border-term-accent focus:outline-none disabled:opacity-50"
                        />
                        <button
                          type="submit"
                          disabled={respondingItemId === item.id || !customResponses[item.id]?.trim()}
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
          {/each}
        </div>
      {/if}
    </div>
  </div>
{/if}
