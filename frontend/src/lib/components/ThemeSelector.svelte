<script lang="ts">
  import { themeStore, currentTheme, themes, type ThemeName } from '$lib/stores/theme';

  let isOpen = $state(false);

  function selectTheme(theme: ThemeName) {
    themeStore.setTheme(theme);
    isOpen = false;
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      isOpen = false;
    }
  }
</script>

<svelte:window onkeydown={handleKeydown} />

<div class="relative">
  <button
    onclick={() => (isOpen = !isOpen)}
    class="flex items-center gap-2 border border-term-border bg-term-bg-secondary px-3 py-1.5 text-sm text-term-fg hover:border-term-accent"
  >
    <span class="text-term-accent">$</span>
    <span>theme</span>
    <span class="text-term-fg-muted">{$currentTheme}</span>
    <svg class="h-4 w-4 text-term-fg-muted" fill="none" viewBox="0 0 24 24" stroke="currentColor">
      <path stroke-linecap="square" stroke-linejoin="miter" stroke-width="2" d="M19 9l-7 7-7-7" />
    </svg>
  </button>

  {#if isOpen}
    <!-- Backdrop to close dropdown -->
    <button
      class="fixed inset-0 z-40"
      onclick={() => (isOpen = false)}
      aria-label="Close theme selector"
    ></button>

    <div
      class="absolute right-0 top-full z-50 mt-1 min-w-48 border border-term-border bg-term-bg shadow-lg"
    >
      {#each themes as theme}
        <button
          onclick={() => selectTheme(theme.id)}
          class="flex w-full items-center gap-3 px-3 py-2 text-left text-sm hover:bg-term-selection"
          class:bg-term-selection={$currentTheme === theme.id}
        >
          <span class="h-3 w-3 border border-term-border" style="background-color: {theme.accent}"
          ></span>
          <span class="text-term-fg">{theme.name}</span>
          {#if $currentTheme === theme.id}
            <span class="ml-auto text-term-accent">*</span>
          {/if}
        </button>
      {/each}
    </div>
  {/if}
</div>
