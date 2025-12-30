<script lang="ts">
  import { tasks, activeTasksCount } from '$lib/stores/tasks';

  function handleClick() {
    tasks.toggle();
  }
</script>

<button
  onclick={handleClick}
  class="relative p-2 text-term-fg-muted transition-colors hover:text-term-fg"
  aria-label="Open tasks"
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
      d="M5.25 14.25h13.5m-13.5 0a3 3 0 0 1-3-3m3 3a3 3 0 1 0 0 6h13.5a3 3 0 1 0 0-6m-16.5-3a3 3 0 0 1 3-3h13.5a3 3 0 0 1 3 3m-19.5 0a4.5 4.5 0 0 1 .9-2.7L5.737 5.1a3.375 3.375 0 0 1 2.7-1.35h7.126c1.062 0 2.062.5 2.7 1.35l2.587 3.45a4.5 4.5 0 0 1 .9 2.7m0 0a3 3 0 0 1-3 3m0 3h.008v.008h-.008v-.008Zm0-6h.008v.008h-.008v-.008Zm-3 6h.008v.008h-.008v-.008Zm0-6h.008v.008h-.008v-.008Z"
    />
  </svg>
  {#if $activeTasksCount > 0}
    <span
      class="absolute -right-1 -top-1 flex h-5 min-w-5 items-center justify-center border border-term-info bg-term-info px-1 text-xs text-term-bg"
    >
      {$activeTasksCount > 99 ? '99+' : $activeTasksCount}
    </span>
  {/if}
</button>
