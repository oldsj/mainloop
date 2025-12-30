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
    class="flex h-8 w-8 items-center justify-center border border-term-border bg-term-bg-secondary text-term-fg-muted hover:border-term-accent hover:text-term-accent"
    aria-label="Change theme"
    title="Change theme"
  >
    <svg class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.5">
      <path stroke-linecap="square" stroke-linejoin="miter" d="M4.098 19.902a3.75 3.75 0 0 0 5.304 0l6.401-6.402M6.75 21A3.75 3.75 0 0 1 3 17.25V4.125C3 3.504 3.504 3 4.125 3h5.25c.621 0 1.125.504 1.125 1.125v4.072M6.75 21a3.75 3.75 0 0 0 3.75-3.75V8.197M6.75 21h13.125c.621 0 1.125-.504 1.125-1.125v-5.25c0-.621-.504-1.125-1.125-1.125h-4.072M10.5 8.197l2.88-2.88c.438-.439 1.15-.439 1.59 0l3.712 3.713c.44.44.44 1.152 0 1.59l-2.879 2.88M6.75 17.25h.008v.008H6.75v-.008Z" />
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
      class="absolute right-0 top-full z-50 mt-1 min-w-40 border border-term-border bg-term-bg shadow-lg"
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
