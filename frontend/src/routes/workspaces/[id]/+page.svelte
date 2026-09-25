<script lang="ts">
  import { page } from '$app/stores';
  import { goto } from '$app/navigation';
  import { get } from 'svelte/store';
  import {
    api,
    type CredentialReauthStatus,
    type WorkspaceCredentialStatus,
    type WorkspaceLifecycle,
    type WorkspacePreviewPort
  } from '$lib/api';
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
  let credentialsError = $state<string | null>(null);
  let credentialStatuses = $state<WorkspaceCredentialStatus[]>([]);
  let reauthJobs = $state<Record<string, CredentialReauthStatus>>({});
  let reauthErrors = $state<Record<string, string>>({});
  let pendingReauth = $state<string | null>(null);
  let pendingAction = $state<WorkspaceAction | null>(null);

  const workspace = $derived(
    $workspaces.workspaces.find((item) => item.workspace_id === workspaceId)
  );
  const operationCondition = $derived(
    workspace?.conditions.find((condition) => condition.type === 'ControlOperation')
  );
  const failedCondition = $derived(
    workspace?.observed_state === 'failed'
      ? workspace.conditions.find((condition) => condition.type === 'Available')
      : workspace?.conditions.find(
          (condition) => condition.type === 'ControlOperation' && condition.status === 'False'
        )
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
    credentialsError = null;
    credentialStatuses = [];
    reauthJobs = {};
    reauthErrors = {};
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
      void loadCredentialStatuses(id);
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

  async function loadCredentialStatuses(id: string) {
    try {
      const statuses = await api.getWorkspaceCredentials(id);
      if (id === workspaceId) credentialStatuses = statuses;
    } catch {
      if (id === workspaceId) credentialsError = 'Credential status could not be loaded.';
    }
  }

  async function startSignIn(provider: 'codex' | 'claude') {
    if (!workspaceId || pendingReauth) return;
    const id = workspaceId;
    pendingReauth = provider;
    reauthErrors = { ...reauthErrors, [provider]: '' };
    try {
      let job = await api.startWorkspaceCredentialReauth(id, provider);
      reauthJobs = { ...reauthJobs, [provider]: job };
      for (let attempt = 0; attempt < 900 && job.state === 'running'; attempt += 1) {
        await new Promise((resolve) => setTimeout(resolve, 2000));
        if (id !== $page.params.id) break;
        job = await api.getWorkspaceCredentialReauth(id, job.id);
        reauthJobs = { ...reauthJobs, [provider]: job };
      }
      if (job.state === 'completed') await loadCredentialStatuses(id);
      else if (job.state === 'failed') reauthErrors = { ...reauthErrors, [provider]: 'Sign-in failed. You can try again.' };
    } catch (error) {
      reauthErrors = {
        ...reauthErrors,
        [provider]: error instanceof Error ? error.message : 'Sign-in could not be started.'
      };
    } finally {
      pendingReauth = null;
    }
  }

  async function runAction(action: WorkspaceAction) {
    if (!workspace) return;
    if (action === 'delete') {
      if (!window.confirm('Delete this branch workspace and its actor?')) return;
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
          'Substrate has not confirmed this lifecycle state. Refresh status before retrying.';
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

  function formatList(items: string[]): string {
    return items.length ? items.join(', ') : 'None declared';
  }

  function conditionColor(status: 'True' | 'False' | 'Unknown'): string {
    if (status === 'True') return 'text-term-green';
    if (status === 'False') return 'text-term-red';
    return 'text-term-yellow';
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
              Desired <span class="text-term-fg">{workspace.desired_state}</span>
              <span class="px-1">·</span>
              Observed <span class="text-term-fg">{workspace.observed_state}</span>
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
        {:else if failedCondition}
          <p class="border-term-red text-term-red mt-3 border-l-2 px-3 py-2 text-sm" role="alert">
            {failedCondition.message}
          </p>
        {:else if operationCondition?.status === 'Unknown'}
          <p
            class="border-term-yellow text-term-yellow mt-3 border-l-2 px-3 py-2 text-sm"
            role="status"
          >
            {operationCondition.message}
          </p>
        {/if}

        {#if workspace.snapshot_ref}
          <p class="text-term-fg-muted mt-3 text-xs break-all">
            Last observed snapshot: <code class="text-term-fg">{workspace.snapshot_ref}</code>
          </p>
        {/if}
        {#if workspace.last_transition}
          <p class="text-term-fg-muted mt-2 text-xs">
            Last transition: {workspace.last_transition.from_state ?? 'new'} →
            {workspace.last_transition.to_state} · {workspace.last_transition.reason} ·
            {formatTime(workspace.last_transition.occurred_at)}
          </p>
        {/if}
      </section>

      <section class="border-term-border border-b py-5" aria-labelledby="conditions-heading">
        <h2 id="conditions-heading" class="text-base font-medium">Conditions</h2>
        {#if workspace.conditions.length}
          <ul class="divide-term-border mt-3 divide-y">
            {#each workspace.conditions as condition (condition.type)}
              <li class="grid gap-1 py-3 sm:grid-cols-[10rem_6rem_minmax(0,1fr)] sm:gap-3">
                <span class="text-sm">{condition.type}</span>
                <span class="text-xs font-medium {conditionColor(condition.status)}"
                  >{condition.status}</span
                >
                <div class="min-w-0">
                  <p class="text-sm break-words">{condition.message}</p>
                  <p class="text-term-fg-muted mt-1 text-xs break-words">
                    {condition.reason} · {formatTime(condition.last_transition_time)}
                  </p>
                </div>
              </li>
            {/each}
          </ul>
        {:else}
          <p class="text-term-fg-muted mt-3 text-sm">No lifecycle conditions have been reported.</p>
        {/if}
      </section>

      <section class="border-b border-term-border py-5" aria-labelledby="credentials-heading">
        <h2 id="credentials-heading" class="text-base font-medium">Agent sign-in</h2>
        <p class="mt-1 text-sm text-term-fg-muted">
          Sign-in credentials stay in the Mainloop control plane; workspaces receive synthetic local auth files.
        </p>
        {#if credentialsError}
          <p class="mt-3 text-sm text-term-yellow" role="status">{credentialsError}</p>
        {:else}
          <ul class="mt-3 divide-y divide-term-border">
            {#each ['codex', 'claude'] as provider}
              {@const status = credentialStatuses.find((item) => item.provider === provider)}
              {@const job = reauthJobs[provider]}
              <li class="flex flex-wrap items-start justify-between gap-3 py-3">
                <div>
                  <p class="text-sm font-medium capitalize">{provider}</p>
                  <p class="mt-1 text-xs text-term-fg-muted">
                    {status?.available ? 'Signed in' : 'Needs sign-in'}
                    {#if status?.expires_at}
                      <span> · expires {formatTime(status.expires_at)}</span>
                    {/if}
                  </p>
                  {#if job?.challenge}
                    <p class="mt-2 text-sm">
                      <a
                        href={job.challenge.url}
                        target="_blank"
                        rel="noopener noreferrer"
                        class="text-term-accent underline underline-offset-4"
                      >Open sign-in page</a>
                      <span class="ml-2">Code: <code class="font-mono">{job.challenge.code}</code></span>
                    </p>
                  {:else if job?.state === 'running'}
                    <p class="mt-2 text-xs text-term-fg-muted" role="status">Waiting for sign-in instructions…</p>
                  {/if}
                  {#if reauthErrors[provider]}
                    <p class="mt-2 text-xs text-term-yellow" role="alert">{reauthErrors[provider]}</p>
                  {/if}
                  {#if job?.state === 'completed'}
                    <p class="mt-2 text-xs text-term-green" role="status">Sign-in completed.</p>
                  {/if}
                </div>
                <button
                  type="button"
                  class="border border-term-border px-3 py-2 text-sm hover:border-term-accent hover:text-term-accent focus-visible:outline focus-visible:outline-2 focus-visible:outline-term-accent disabled:cursor-not-allowed disabled:opacity-50"
                  disabled={pendingReauth !== null}
                  onclick={() => startSignIn(provider as 'codex' | 'claude')}
                  aria-busy={pendingReauth === provider}
                >
                  {pendingReauth === provider ? 'Waiting for sign-in…' : 'Sign in'}
                </button>
              </li>
            {/each}
          </ul>
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
            <dt class="text-term-fg-muted text-xs">Agent kinds</dt>
            <dd class="mt-1 text-sm">{formatList(workspace.manifest.agent_kinds)}</dd>
          </div>
          <div class="border-term-border border-b py-3">
            <dt class="text-term-fg-muted text-xs">Resource class</dt>
            <dd class="mt-1 font-mono text-sm">{workspace.manifest.resource_class}</dd>
          </div>
          <div class="border-term-border border-b py-3">
            <dt class="text-term-fg-muted text-xs">Idle timeout</dt>
            <dd class="mt-1 text-sm">
              {workspace.manifest.dev?.idle_timeout_minutes ?? 'Not configured'} minutes
            </dd>
          </div>
          <div class="border-term-border border-b py-3">
            <dt class="text-term-fg-muted text-xs">Last activity</dt>
            <dd class="mt-1 text-sm">
              {workspace.last_activity_at ? formatTime(workspace.last_activity_at) : 'Not recorded'}
            </dd>
          </div>
          {#if workspace.manifest.dev}
            <div class="border-term-border border-b py-3">
              <dt class="text-term-fg-muted text-xs">Development image</dt>
              <dd class="mt-1 font-mono text-sm break-all">
                {workspace.manifest.dev.image ?? workspace.manifest.dev.devcontainer_ref}
              </dd>
            </div>
            <div class="border-term-border border-b py-3">
              <dt class="text-term-fg-muted text-xs">Actor template</dt>
              <dd class="mt-1 font-mono text-sm">
                {workspace.manifest.dev.actor_template ?? 'Default project template'}
              </dd>
            </div>
            <div class="border-term-border border-b py-3 sm:col-span-2">
              <dt class="text-term-fg-muted text-xs">Services</dt>
              <dd class="mt-1 text-sm break-words">
                {workspace.manifest.dev.services.length
                  ? workspace.manifest.dev.services
                      .map((service) => `${service.name} (${service.image})`)
                      .join(', ')
                  : 'None declared'}
              </dd>
            </div>
            <div class="border-term-border border-b py-3 sm:col-span-2">
              <dt class="text-term-fg-muted text-xs">Preview ports</dt>
              <dd class="mt-1 text-sm break-words">
                {workspace.manifest.dev.ports.length
                  ? workspace.manifest.dev.ports
                      .map((port) => `${port.name}: ${port.number}/${port.protocol}`)
                      .join(', ')
                  : 'None declared'}
              </dd>
            </div>
          {/if}
          <div class="border-term-border border-b py-3">
            <dt class="text-term-fg-muted text-xs">Skills (references)</dt>
            <dd class="mt-1 text-sm break-words">{formatList(workspace.manifest.skills)}</dd>
          </div>
          <div class="border-term-border border-b py-3">
            <dt class="text-term-fg-muted text-xs">MCP servers (references)</dt>
            <dd class="mt-1 text-sm break-words">{formatList(workspace.manifest.mcp_servers)}</dd>
          </div>
          <div class="border-term-border border-b py-3 sm:col-span-2">
            <dt class="text-term-fg-muted text-xs">Egress host allowlist</dt>
            <dd class="mt-1 font-mono text-sm break-words">
              {formatList(workspace.manifest.egress_allowlist)}
            </dd>
          </div>
        </dl>
      </section>

      <section class="border-t border-term-border py-5" aria-labelledby="previews-heading">
        <h2 id="previews-heading" class="text-base font-medium">Previews</h2>
        <p class="mt-1 text-sm text-term-fg-muted">
          Open a declared or currently listening HTTP port in a new tab.
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
          <p class="mt-3 text-sm text-term-fg-muted">No declared or listening HTTP ports are available.</p>
        {/if}
      </section>
    </div>
  </main>
{/if}
