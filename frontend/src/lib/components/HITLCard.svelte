<script lang="ts">
  import { hitlClient, watchHITL } from '../hitlClient';
  import type { HITLState } from '../hitl';
  import HITLRequest from './HITLRequest.svelte';
  let { requestId }: { requestId: string } = $props();
  let state = $state<HITLState>({
    view: null,
    busy: false,
    error: null,
    uncertain: false,
    stale: true
  });
  $effect(() => {
    const id = requestId;
    return watchHITL(id, (next) => (state = next));
  });
</script>

{#key requestId}
  <HITLRequest
    snapshot={state}
    onRespond={(draft) => void hitlClient.respond(requestId, draft)}
    onRefresh={() => void hitlClient.refresh(requestId)}
  />
{/key}
