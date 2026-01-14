<script lang="ts">
  import { draftMessage } from '$lib/stores/draftMessage';
  import { navigationContext, currentSession } from '$lib/stores/navigationContext';

  let {
    disabled = false,
    onsend,
    placeholder = 'Enter command...',
    sessionColor = null
  }: {
    disabled?: boolean;
    onsend?: (detail: { message: string }) => void;
    placeholder?: string;
    sessionColor?: string | null;
  } = $props();

  // Derive border color from current session context
  const borderColor = $derived(sessionColor ?? $currentSession?.color ?? null);

  function handleSubmit(event: SubmitEvent) {
    event.preventDefault();
    if ($draftMessage.trim() && !disabled && onsend) {
      onsend({ message: $draftMessage.trim() });
      draftMessage.set('');
    }
  }

  function handleKeydown(event: KeyboardEvent) {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault();
      handleSubmit(event as any);
    }
  }
</script>

<div>
  <form
    data-testid="input-bar"
    onsubmit={handleSubmit}
    class="flex items-center gap-2 border border-term-border bg-term-bg-secondary px-3 py-2"
    style={borderColor ? `border-left: 4px solid ${borderColor};` : ''}
  >
    <span class="shrink-0 text-term-accent">$</span>
    <textarea
      data-testid="command-input"
      bind:value={$draftMessage}
      onkeydown={handleKeydown}
      {disabled}
      {placeholder}
      rows="1"
      class="flex-1 resize-none border-none bg-transparent text-term-fg placeholder:text-term-fg-muted focus:outline-none disabled:opacity-50"
    ></textarea>
    <button
      data-testid="exec-button"
      type="submit"
      {disabled}
      class="border border-term-border bg-term-bg px-4 py-1 text-term-fg hover:border-term-accent hover:text-term-accent disabled:opacity-50 disabled:hover:border-term-border disabled:hover:text-term-fg"
    >
      EXEC
    </button>
  </form>

  {#if $currentSession}
    <div class="mt-1 flex items-center justify-between px-3 text-xs text-term-fg-muted">
      <span>
        Replying to <span style="color: {$currentSession.color}">{$currentSession.title}</span>
      </span>
      <button
        type="button"
        class="hover:text-term-accent"
        onclick={() => navigationContext.switchToMain()}
      >
        [Shift+Tab to exit]
      </button>
    </div>
  {/if}
</div>
