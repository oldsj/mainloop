<script lang="ts">
  import { page } from '$app/stores';
  import { goto } from '$app/navigation';
  import { get } from 'svelte/store';
  import { api, type WorkspaceLifecycle, type WorkspacePreviewPort } from '$lib/api';
  import WorkspaceLifecycleBadge from '$lib/components/WorkspaceLifecycleBadge.svelte';
  import { connection } from '$lib/stores/connection';
  import { workspaces } from '$lib/stores/workspaces';

  type WorkspaceAction = 'suspend' | 'resume' | 'refresh' | 'delete';

  let workspaceId = $derived($page.params.id);
  let loading = $state(false);
  let unreachable = $state(false);
  let pageError = $state<string | null>(null);
  let actionError = $state<string | null>(null);
  let previewPortsError = $state<string | null>(null);
  let previewPorts = $state<WorkspacePreviewPort[]>([]);
  let pendingAction = $state<WorkspaceAction | null>(null);

  const workspace = $derived(
    $workspaces.workspaces.find((item) => item.workspace_id === workspaceId)
  );
  const isBusy = $derived(pendingAction !== null);

  $effect(() => {
    const id = workspaceId;
    if (!id) return;
    pageError = null;
    unreachable = false;
    actionError = null;
    previewPortsError = null;
    previewPorts = [];
    void loadWorkspace(id);
  });

  let seenRecoveries = get(connection).recoveries;
  $effect(() => {
    const recoveries = $connection.recoveries;
    if (recoveries === seenRecoveries) return;
    seenRecoveries = recoveries;
    if (unreachable && workspaceId) {
      pageError = null;
      unreachable = false;
      void loadWorkspace(workspaceId);
    }
  });

  async function loadWorkspace(id: string) {
    loading = true;
    try {
      const result = await api.getWorkspace(id);
      if (id === workspaceId) workspaces.upsert(result);
      void loadPreviewPorts(id);
    } catch (error) {
      if (id !== workspaceId) return;
      unreachable = error instanceof TypeError || get(connection).status === 'offline';
      pageError = unreachable ? "Can't reach the Mainloop backend." : 'Workspace not found';
    } finally {
      if (id === workspaceId) loading = false;
    }
  }

  async function loadPreviewPorts(id: string) {
    try {
      const ports = await api.listWorkspacePreviewPorts(id);
      if (id === workspaceId) previewPorts = ports;
    } catch {
      if (id === workspaceId) previewPortsError = 'Preview ports could not be loaded.';
    }
  }

  async function runAction(action: WorkspaceAction) {
    if (!workspace) return;
    if (action === 'delete') {
      if (!window.confirm('Delete this branch workspace? Its checkout and any uncommitted work are removed.')) return;
      pendingAction = action;
      actionError = null;
      try {
        await api.deleteWorkspace(workspace.workspace_id);
        await goto('/');
      } catch (error) {
        actionError = error instanceof Error ? error.message : 'Failed to delete workspace';
      } finally {
        pendingAction = null;
      }
      return;
    }
    pendingAction = action;
    actionError = null;
    try {
      let result: WorkspaceLifecycle;
      if (action === 'suspend') result = await api.suspendWorkspace(workspace.workspace_id);
      else if (action === 'resume') result = await api.resumeWorkspace(workspace.workspace_id);
      else result = await api.refreshWorkspace(workspace.workspace_id);
      workspaces.upsert(result);
      if (result.observed_state === 'unknown') {
        actionError =
          'kagent has not confirmed this lifecycle state. Refresh status before retrying.';
      }
    } catch (error) {
      actionError = error instanceof Error ? error.message : `Failed to ${action} workspace`;
    } finally {
      pendingAction = null;
    }
  }

  function formatTime(value: string): string {
    return new Date(value).toLocaleString();
  }

</script>

<svelte:head>
  <title>{workspace ? `Workspace ${workspace.workspace_id}` : 'Workspace'} - mainloop</title>
</svelte:head>

{#if pageError}
  <main class="bg-term-bg text-term-fg flex h-full items-center justify-center px-4">
    <div class="max-w-lg text-center">
      <h1 class="text-lg font-medium">{pageError}</h1>
      {#if unreachable}
        <p class="text-term-fg-muted mt-2 text-sm">
          The page will retry when the backend reconnects.
        </p>
      {/if}
      <a href="/" class="text-term-accent mt-4 inline-block underline underline-offset-4"
        >Back to sessions</a
      >
    </div>
  </main>
{:else if loading && !workspace}
  <main class="bg-term-bg text-term-fg h-full overflow-y-auto p-4 sm:p-6" aria-busy="true">
    <div class="mx-auto max-w-4xl animate-pulse space-y-4" aria-label="Loading workspace">
      <div class="bg-term-bg-secondary h-6 w-40"></div>
      <div class="border-term-border bg-term-bg-secondary/50 h-24 border"></div>
      <div class="border-term-border bg-term-bg-secondary/50 h-48 border"></div>
    </div>
  </main>
{:else if workspace}
  <main class="bg-term-bg text-term-fg h-full overflow-y-auto px-4 py-5 sm:px-6 sm:py-7">
    <div class="mx-auto max-w-4xl">
      <header
        class="border-term-border flex flex-wrap items-start justify-between gap-4 border-b pb-5"
      >
        <div class="min-w-0">
          <a
            href={`/sessions/${workspace.session_id}`}
            class="text-term-fg-muted hover:text-term-accent text-xs underline underline-offset-4"
          >
            Back to session
          </a>
          <h1 class="mt-3 text-xl font-medium">Workspace</h1>
          <p class="text-term-fg-muted mt-1 font-mono text-xs break-all">
            {workspace.workspace_id}
          </p>
        </div>
        <WorkspaceLifecycleBadge {workspace} />
      </header>

      <section class="border-term-border border-b py-5" aria-labelledby="lifecycle-heading">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div>
            <h2 id="lifecycle-heading" class="text-base font-medium">Lifecycle</h2>
            <p class="text-term-fg-muted mt-1 text-sm">
              State <span class="text-term-fg">{workspace.observed_state}</span>
            </p>
          </div>
          <div class="flex flex-wrap gap-2">
            <button
              type="button"
              class="border-term-border text-term-fg hover:border-term-accent hover:text-term-accent focus-visible:outline-term-accent border px-3 py-2 text-sm focus-visible:outline focus-visible:outline-2 disabled:cursor-not-allowed disabled:opacity-50"
              disabled={isBusy || workspace.observed_state !== 'running'}
              onclick={() => runAction('suspend')}
              aria-busy={pendingAction === 'suspend'}
            >
              {pendingAction === 'suspend' ? 'Suspending…' : 'Suspend workspace'}
            </button>
            <button
              type="button"
              class="border-term-red/60 text-term-red hover:border-term-red focus-visible:outline-term-red border px-3 py-2 text-sm focus-visible:outline focus-visible:outline-2 disabled:opacity-50"
              disabled={isBusy}
              onclick={() => runAction('delete')}
            >
              {pendingAction === 'delete' ? 'Deleting…' : 'Delete workspace'}
            </button>
            <button
              type="button"
              class="border-term-accent bg-term-accent/10 text-term-accent hover:bg-term-accent/20 focus-visible:outline-term-accent border px-3 py-2 text-sm focus-visible:outline focus-visible:outline-2 disabled:cursor-not-allowed disabled:opacity-50"
              disabled={isBusy || workspace.observed_state !== 'suspended'}
              onclick={() => runAction('resume')}
              aria-busy={pendingAction === 'resume'}
            >
              {pendingAction === 'resume' ? 'Resuming…' : 'Resume workspace'}
            </button>
            <button
              type="button"
              class="border-term-border text-term-fg-muted hover:border-term-accent hover:text-term-accent focus-visible:outline-term-accent border px-3 py-2 text-sm focus-visible:outline focus-visible:outline-2 disabled:cursor-not-allowed disabled:opacity-50"
              disabled={isBusy}
              onclick={() => runAction('refresh')}
              aria-busy={pendingAction === 'refresh'}
            >
              {pendingAction === 'refresh' ? 'Refreshing…' : 'Refresh status'}
            </button>
          </div>
        </div>

        {#if actionError}
          <p
            class="border-term-yellow text-term-yellow mt-3 border-l-2 px-3 py-2 text-sm"
            role="alert"
          >
            {actionError}
          </p>
        {:else if workspace.observed_state === 'failed' && workspace.detail}
          <p class="border-term-red text-term-red mt-3 border-l-2 px-3 py-2 text-sm" role="alert">
            {workspace.detail}
          </p>
        {:else if workspace.detail}
          <p
            class="border-term-yellow text-term-yellow mt-3 border-l-2 px-3 py-2 text-sm"
            role="status"
          >
            {workspace.detail}
          </p>
        {/if}
      </section>

      <section class="py-5" aria-labelledby="manifest-heading">
        <div class="border-term-border border-b pb-3">
          <h2 id="manifest-heading" class="text-base font-medium">Workspace manifest</h2>
          <p class="text-term-fg-muted mt-1 text-xs">
            Project settings applied to this branch workspace.
          </p>
        </div>
        <dl class="grid gap-x-6 sm:grid-cols-2">
          <div class="border-term-border border-b py-3">
            <dt class="text-term-fg-muted text-xs">Repository</dt>
            <dd class="mt-1 text-sm break-all">
              {#if workspace.manifest.repo_url}
                <a
                  href={workspace.manifest.repo_url}
                  target="_blank"
                  rel="noopener noreferrer"
                  class="text-term-accent underline underline-offset-4"
                >
                  {workspace.manifest.repo_url}
                </a>
              {:else}
                Not declared
              {/if}
            </dd>
          </div>
          <div class="border-term-border border-b py-3">
            <dt class="text-term-fg-muted text-xs">Branch</dt>
            <dd class="mt-1 font-mono text-sm break-all">{workspace.manifest.branch}</dd>
          </div>
          <div class="border-term-border border-b py-3">
            <dt class="text-term-fg-muted text-xs">Base ref</dt>
            <dd class="mt-1 font-mono text-sm break-all">{workspace.manifest.ref || 'Default'}</dd>
          </div>
          <div class="border-term-border border-b py-3">
            <dt class="text-term-fg-muted text-xs">Agent</dt>
            <dd class="mt-1 text-sm">{workspace.manifest.agent_kind}</dd>
          </div>
          <div class="border-term-border border-b py-3">
            <dt class="text-term-fg-muted text-xs">Idle timeout</dt>
            <dd class="mt-1 text-sm">{workspace.manifest.dev.idle_timeout_minutes} minutes</dd>
          </div>
          <div class="border-term-border border-b py-3">
            <dt class="text-term-fg-muted text-xs">Last activity</dt>
            <dd class="mt-1 text-sm">
              {workspace.last_activity_at ? formatTime(workspace.last_activity_at) : 'Not recorded'}
            </dd>
          </div>
          <div class="border-term-border border-b py-3 sm:col-span-2">
            <dt class="text-term-fg-muted text-xs">Preview ports</dt>
            <dd class="mt-1 text-sm break-words">
              {workspace.manifest.dev.ports.length
                ? workspace.manifest.dev.ports.map((port) => `${port.name}: ${port.number}`).join(', ')
                : 'None declared'}
            </dd>
          </div>
        </dl>
      </section>

      <section class="border-t border-term-border py-5" aria-labelledby="previews-heading">
        <h2 id="previews-heading" class="text-base font-medium">Previews</h2>
        <p class="mt-1 text-sm text-term-fg-muted">
          Open a declared HTTP port in a new tab. A suspended workspace wakes when you open it.
        </p>
        {#if previewPortsError}
          <p class="mt-3 text-sm text-term-yellow" role="status">{previewPortsError}</p>
        {:else if previewPorts.length}
          <ul class="mt-3 divide-y divide-term-border">
            {#each previewPorts as previewPort (previewPort.port)}
              <li class="flex flex-wrap items-center justify-between gap-3 py-3">
                <span class="text-sm">
                  {previewPort.name}
                  <span class="ml-2 font-mono text-xs text-term-fg-muted">:{previewPort.port}</span>
                </span>
                <a
                  href={previewPort.url}
                  target="_blank"
                  rel="noopener noreferrer"
                  class="border border-term-accent px-3 py-2 text-sm text-term-accent hover:bg-term-accent/10 focus-visible:outline focus-visible:outline-2 focus-visible:outline-term-accent"
                >
                  Open preview
                </a>
              </li>
            {/each}
          </ul>
        {:else}
          <p class="mt-3 text-sm text-term-fg-muted">No preview ports are declared.</p>
        {/if}
      </section>
    </div>
  </main>
{/if}
