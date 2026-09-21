<script lang="ts">
  import { connection } from '$lib/stores/connection';

  const since = $derived(
    $connection.offlineSince
      ? new Date($connection.offlineSince).toLocaleTimeString([], {
          hour: '2-digit',
          minute: '2-digit'
        })
      : null
  );
</script>

{#if $connection.status === 'offline'}
  <div
    class="border-term-red/60 bg-term-red/10 text-term-red flex shrink-0 items-center gap-2 border-b px-4 py-2 text-sm"
    role="alert"
    data-testid="connection-banner"
  >
    <span class="h-2 w-2 shrink-0 animate-pulse rounded-full bg-current" aria-hidden="true"></span>
    <span>
      Can't reach the Mainloop backend{since ? ` since ${since}` : ''}. Retrying — what you see may
      be out of date and messages can't be sent.
    </span>
  </div>
{/if}
