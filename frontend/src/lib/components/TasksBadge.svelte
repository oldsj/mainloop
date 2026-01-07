<script lang="ts">
  import { tasks, activeTasksCount } from '$lib/stores/tasks';
  import { unreadCount } from '$lib/stores/inbox';

  // Combined count for badge
  const totalCount = $derived($activeTasksCount + $unreadCount);

  function handleClick() {
    tasks.toggle();
  }
</script>

<button
  type="button"
  onclick={handleClick}
  class="relative p-2 text-term-fg-muted transition-colors hover:text-term-fg"
  aria-label="Open inbox"
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
      class="absolute -right-1 -top-1 flex h-5 min-w-5 items-center justify-center border border-term-info bg-term-info px-1 text-xs text-term-bg"
    >
      {totalCount > 99 ? '99+' : totalCount}
    </span>
  {/if}
</button>
