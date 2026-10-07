<script lang="ts">
  import { visiblePolling } from '../visiblePolling';
  import { api } from '../api';
  import HITLCard from './HITLCard.svelte';
  let { sessionId }: { sessionId: string } = $props();
  let ids = $state<string[]>([]);
  let error = $state(false);
  $effect(() => {
    const id = sessionId;
    ids = [];
    error = false;
    let stopped = false,
      loading = false;
    async function refresh(signal: AbortSignal) {
      if (loading) return;
      loading = true;
      try {
        const result = await api.listSessionHITL(id, signal);
        if (!stopped && !signal.aborted) {
          ids = [...new Set(result)];
          error = false;
        }
      } catch {
        if (!stopped && (!signal.aborted || signal.reason?.name === 'TimeoutError')) error = true;
      } finally {
        loading = false;
      }
    }
    const stop = visiblePolling().watch(Symbol(`session:${id}`), refresh);
    return () => {
      stopped = true;
      stop();
    };
  });
</script>

{#if ids.length || error}
  <div
    class="border-term-border max-h-[50vh] shrink-0 overflow-y-auto border-b"
    aria-label="Session requests"
  >
    {#if error}<p class="text-term-fg-muted p-4 text-sm" role="status">
        Could not refresh session requests. Retrying…
      </p>{/if}
    {#each ids as id (id)}<HITLCard requestId={id} />{/each}
  </div>
{/if}
