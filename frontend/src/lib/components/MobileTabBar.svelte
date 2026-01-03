<script lang="ts">
  import { activeTasksCount } from '$lib/stores/tasks';
  import { unreadCount } from '$lib/stores/inbox';

  type Tab = 'chat' | 'tasks';

  let { activeTab = $bindable('chat') }: { activeTab: Tab } = $props();

  const totalCount = $derived($activeTasksCount + $unreadCount);
</script>

<nav
  data-testid="mobile-tab-bar"
  class="fixed bottom-0 left-0 right-0 z-40 border-t border-term-border bg-term-bg md:hidden"
>
  <div class="flex items-center justify-around">
    <button
      data-testid="tab-chat"
      onclick={() => (activeTab = 'chat')}
      class="flex flex-1 flex-col items-center gap-1 py-3 transition-colors"
      class:text-term-accent={activeTab === 'chat'}
      class:text-term-fg-muted={activeTab !== 'chat'}
      aria-label="Chat"
      aria-selected={activeTab === 'chat'}
    >
      <svg
        xmlns="http://www.w3.org/2000/svg"
        fill="none"
        viewBox="0 0 24 24"
        stroke-width="1.5"
        stroke="currentColor"
        class="h-6 w-6"
      >
        <path
          stroke-linecap="square"
          stroke-linejoin="miter"
          d="M8.625 12a.375.375 0 1 1-.75 0 .375.375 0 0 1 .75 0Zm0 0H8.25m4.125 0a.375.375 0 1 1-.75 0 .375.375 0 0 1 .75 0Zm0 0H12m4.125 0a.375.375 0 1 1-.75 0 .375.375 0 0 1 .75 0Zm0 0h-.375M21 12c0 4.556-4.03 8.25-9 8.25a9.764 9.764 0 0 1-2.555-.337A5.972 5.972 0 0 1 5.41 20.97a5.969 5.969 0 0 1-.474-.065 4.48 4.48 0 0 0 .978-2.025c.09-.457-.133-.901-.467-1.226C3.93 16.178 3 14.189 3 12c0-4.556 4.03-8.25 9-8.25s9 3.694 9 8.25Z"
        />
      </svg>
      <span class="text-xs">[CHAT]</span>
    </button>

    <button
      data-testid="tab-inbox"
      onclick={() => (activeTab = 'tasks')}
      class="relative flex flex-1 flex-col items-center gap-1 py-3 transition-colors"
      class:text-term-accent={activeTab === 'tasks'}
      class:text-term-fg-muted={activeTab !== 'tasks'}
      aria-label="Inbox"
      aria-selected={activeTab === 'tasks'}
    >
      <svg
        xmlns="http://www.w3.org/2000/svg"
        fill="none"
        viewBox="0 0 24 24"
        stroke-width="1.5"
        stroke="currentColor"
        class="h-6 w-6"
      >
        <path
          stroke-linecap="square"
          stroke-linejoin="miter"
          d="M2.25 13.5h3.86a2.25 2.25 0 0 1 2.012 1.244l.256.512a2.25 2.25 0 0 0 2.013 1.244h3.218a2.25 2.25 0 0 0 2.013-1.244l.256-.512a2.25 2.25 0 0 1 2.013-1.244h3.859m-19.5.338V18a2.25 2.25 0 0 0 2.25 2.25h15A2.25 2.25 0 0 0 21.75 18v-4.162c0-.224-.034-.447-.1-.661L19.24 5.338a2.25 2.25 0 0 0-2.15-1.588H6.911a2.25 2.25 0 0 0-2.15 1.588L2.35 13.177a2.25 2.25 0 0 0-.1.661Z"
        />
      </svg>
      {#if totalCount > 0}
        <span
          data-testid="inbox-badge"
          class="absolute right-1/4 top-2 flex h-5 min-w-5 items-center justify-center border border-term-info bg-term-info px-1 text-xs text-term-bg"
        >
          {totalCount}
        </span>
      {/if}
      <span class="text-xs">[INBOX]</span>
    </button>
  </div>
</nav>
