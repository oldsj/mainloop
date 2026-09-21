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

  const MAX_HEIGHT_PX = 160;

  let textarea = $state<HTMLTextAreaElement>();
  // The box is disabled while a reply is pending, which drops focus; take it back afterwards.
  let refocusWhenEnabled = false;

  // Grow with the text (Shift+Enter adds lines) up to a cap, and shrink back after a send.
  $effect(() => {
    if (!textarea) return;
    // Empty: one row, whatever the placeholder's length (it would otherwise size the box).
    if (!$draftMessage) {
      textarea.style.height = '';
      return;
    }
    textarea.style.height = 'auto';
    textarea.style.height = `${Math.min(textarea.scrollHeight, MAX_HEIGHT_PX)}px`;
  });

  $effect(() => {
    if (disabled || !refocusWhenEnabled || !textarea) return;
    refocusWhenEnabled = false;
    // Only when focus was lost, not when the user has moved on to something else.
    if (document.activeElement === document.body) textarea.focus();
  });

  function handleSubmit(event: SubmitEvent) {
    event.preventDefault();
    if ($draftMessage.trim() && !disabled && onsend) {
      onsend({ message: $draftMessage.trim() });
      draftMessage.set('');
      refocusWhenEnabled = true;
    }
  }

  function handleKeydown(event: KeyboardEvent) {
    // Enter confirms an IME candidate (CJK, etc.); it must not send the half-composed message.
    if (event.isComposing) return;
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
    class="flex items-end gap-2 border border-term-border bg-term-bg-secondary px-3 py-2"
    style={borderColor ? `border-left: 4px solid ${borderColor};` : ''}
  >
    <span class="shrink-0 py-1 text-term-accent">$</span>
    <textarea
      bind:this={textarea}
      data-testid="command-input"
      bind:value={$draftMessage}
      onkeydown={handleKeydown}
      {disabled}
      {placeholder}
      rows="1"
      class="max-h-40 flex-1 resize-none overflow-y-auto border-none bg-transparent py-1 text-term-fg placeholder:text-term-fg-muted focus:outline-none disabled:opacity-50"
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

