<script lang="ts">
  import { inbox, unreadCount } from '$lib/stores/inbox';

  function handleClick() {
    inbox.toggle();
  }
</script>

<button
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
      d="M14.857 17.082a23.848 23.848 0 0 0 5.454-1.31A8.967 8.967 0 0 1 18 9.75V9A6 6 0 0 0 6 9v.75a8.967 8.967 0 0 1-2.312 6.022c1.733.64 3.56 1.085 5.455 1.31m5.714 0a24.255 24.255 0 0 1-5.714 0m5.714 0a3 3 0 1 1-5.714 0"
    />
  </svg>
  {#if $unreadCount > 0}
    <span
      class="absolute -right-1 -top-1 flex h-5 min-w-5 items-center justify-center border border-term-error bg-term-error px-1 text-xs text-term-bg"
    >
      {$unreadCount > 99 ? '99+' : $unreadCount}
    </span>
  {/if}
</button>
