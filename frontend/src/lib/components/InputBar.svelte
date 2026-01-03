<script lang="ts">
  let {
    disabled = false,
    onsend
  }: { disabled?: boolean; onsend?: (detail: { message: string }) => void } = $props();

  let message = $state('');

  function handleSubmit(event: SubmitEvent) {
    event.preventDefault();
    if (message.trim() && !disabled && onsend) {
      onsend({ message: message.trim() });
      message = '';
    }
  }

  function handleKeydown(event: KeyboardEvent) {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault();
      handleSubmit(event as any);
    }
  }
</script>

<form
  data-testid="input-bar"
  onsubmit={handleSubmit}
  class="flex items-center gap-2 border border-term-border bg-term-bg-secondary px-3 py-2"
>
  <span class="shrink-0 text-term-accent">$</span>
  <textarea
    data-testid="command-input"
    bind:value={message}
    onkeydown={handleKeydown}
    {disabled}
    placeholder="Enter command..."
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
