<script lang="ts">
  let { disabled = false, onsend }: { disabled?: boolean; onsend?: (detail: { message: string }) => void } = $props();

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

<form onsubmit={handleSubmit} class="flex gap-2">
  <textarea
    bind:value={message}
    onkeydown={handleKeydown}
    {disabled}
    placeholder="Type a message..."
    rows="1"
    class="flex-1 resize-none rounded-lg border border-neutral-300 px-4 py-2 focus:border-primary-500 focus:outline-none focus:ring-1 focus:ring-primary-500 disabled:opacity-50"
  />
  <button
    type="submit"
    {disabled}
    class="rounded-lg bg-primary-500 px-6 py-2 text-white hover:bg-primary-600 disabled:opacity-50 disabled:hover:bg-primary-500"
  >
    Send
  </button>
</form>
