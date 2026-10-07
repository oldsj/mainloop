<script lang="ts">
  import { api } from '../api';
  import { hitlClient, watchHITL } from '../hitlClient';
  import type { HITLState } from '../hitl';
  import HITLRequest from './HITLRequest.svelte';
  import { taskViews } from '../stores/tasks';
  import { statusLabel, taskForApproval } from '../taskState';
  let { requestId }: { requestId: string } = $props();
  let state = $state<HITLState>({
    view: null,
    busy: false,
    error: null,
    uncertain: false,
    stale: true
  });
  const owningTask = $derived(taskForApproval($taskViews, requestId));
  $effect(() => {
    const id = requestId;
    return watchHITL(id, (next) => (state = next));
  });
</script>

{#if owningTask}
  <p class="border-term-border border-b px-4 py-2 text-xs" data-testid="hitl-task-link">
    <span class="text-term-fg-muted">Task</span>
    <a href="/tasks/{owningTask.task.id}" class="text-term-accent underline underline-offset-4"
      >{owningTask.task.title}</a
    >
    <span class="text-term-fg-muted">{statusLabel(owningTask.task.status)}</span>
  </p>
{/if}

{#key requestId}
  <HITLRequest
    snapshot={state}
    onRespond={(draft) => void hitlClient.respond(requestId, draft)}
    onRefresh={() => void hitlClient.refresh(requestId)}
    onLoadMergeDetails={(proposalId, section, cursor) =>
      api.getHITLMergeDetails(requestId, proposalId, section, cursor)}
  />
{/key}
