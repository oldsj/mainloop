<script lang="ts">
  import { page } from '$app/stores';
  import { projects, currentProject } from '$lib/stores/projects';
  import { goto } from '$app/navigation';
  import { statusLabel } from '$lib/sessionStatus';
  import { api, type WorkspaceLifecycle } from '$lib/api';

  // The route is reused when only [id] changes, so load per id rather than once on mount.
  const projectId = $derived($page.params.id);
  let branch = $state('');
  let workspaceRows = $state<WorkspaceLifecycle[]>([]);
  let workspaceBusy = $state(false);
  let workspaceError = $state<string | null>(null);

  $effect(() => {
    if (projectId) {
      branch = '';
      projects.fetchProjectDetail(projectId);
      void api
        .listWorkspaces()
        .then((rows) => {
          if (projectId === $page.params.id) workspaceRows = rows;
        })
        .catch(() => {
          workspaceRows = [];
        });
    }
  });

  // The store keeps the last project until the next one arrives; don't show it under another id.
  const detail = $derived($currentProject?.project.id === projectId ? $currentProject : null);
  const projectWorkspaces = $derived(
    workspaceRows.filter((item) => item.manifest.repo_url === detail?.project.html_url)
  );

  function formatDate(dateStr: string): string {
    const date = new Date(dateStr);
    const now = new Date();
    const diff = now.getTime() - date.getTime();
    const minutes = Math.floor(diff / 60000);
    const hours = Math.floor(diff / 3600000);
    const days = Math.floor(diff / 86400000);

    if (minutes < 60) return `${minutes}m ago`;
    if (hours < 24) return `${hours}h ago`;
    if (days < 7) return `${days}d ago`;
    return date.toLocaleDateString();
  }

  function getStatusColor(status: string): string {
    switch (status) {
      case 'completed':
        return 'text-term-green';
      case 'failed':
        return 'text-term-red';
      case 'waiting_on_user':
        return 'text-term-magenta';
      case 'active':
      case 'implementing':
        return 'text-term-cyan';
      case 'pending':
      case 'under_review':
        return 'text-term-yellow';
      default:
        return 'text-term-fg-muted';
    }
  }

  async function createWorkspace(project: { id: string; default_branch: string }) {
    workspaceBusy = true;
    workspaceError = null;
    try {
      const workspace = await api.createWorkspace(
        project.id,
        branch.trim() || project.default_branch,
        {
          image: 'node:22-bookworm',
          devcontainer_ref: null,
          actor_template: null,
          services: [],
          ports: [],
          idle_timeout_minutes: 30
        }
      );
      await goto(`/workspaces/${workspace.workspace_id}`);
    } catch (error) {
      workspaceError = error instanceof Error ? error.message : 'Failed to create workspace';
    } finally {
      workspaceBusy = false;
    }
  }
</script>

<svelte:head>
  <title>{$currentProject?.project.full_name || 'Project'} - mainloop</title>
</svelte:head>

<div class="bg-term-bg flex h-full flex-col overflow-hidden">
  {#if detail}
    {@const { project, open_prs, recent_commits, sessions: projectSessions } = detail}

    <!-- Header -->
    <header class="border-term-border border-b px-6 py-4">
      <button
        type="button"
        onclick={() => goto('/')}
        class="text-term-fg-muted hover:text-term-accent mb-3 flex items-center gap-1 text-xs"
      >
        <svg class="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke="currentColor">
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M15 19l-7-7 7-7"
          />
        </svg>
        Back
      </button>

      <div class="flex items-start gap-3">
        {#if project.avatar_url}
          <img src={project.avatar_url} alt="" class="h-12 w-12 rounded" />
        {/if}
        <div class="flex-1">
          <h1 class="text-term-accent text-2xl font-bold">{project.full_name}</h1>
          {#if project.description}
            <p class="text-term-fg-muted mt-1 text-sm">{project.description}</p>
          {/if}
          <div class="mt-2 flex items-center gap-3">
            <a
              href={project.html_url}
              target="_blank"
              rel="noopener noreferrer"
              class="text-term-fg hover:text-term-accent text-xs"
            >
              View on GitHub →
            </a>
            <span class="text-term-fg-muted text-xs">
              {project.open_pr_count} open {project.open_pr_count === 1 ? 'PR' : 'PRs'}
            </span>
          </div>
        </div>
      </div>
    </header>

    <!-- Content -->
    <div class="flex-1 overflow-y-auto px-6 py-4">
      <!-- Open PRs -->
      <section class="mb-6">
        <h2 class="text-term-fg mb-3 text-sm font-semibold">Open Pull Requests</h2>
        {#if open_prs.length > 0}
          <div class="space-y-2">
            {#each open_prs as pr (pr.number)}
              <a
                href={pr.url}
                target="_blank"
                rel="noopener noreferrer"
                class="border-term-border bg-term-bg hover:border-term-accent block border p-3"
              >
                <div class="flex items-start justify-between gap-2">
                  <div class="flex-1">
                    <div class="flex items-center gap-2">
                      <span class="text-term-fg text-sm font-medium">#{pr.number}</span>
                      {#if pr.is_mainloop}
                        <span class="bg-term-accent text-term-bg rounded px-1.5 py-0.5 text-xs">
                          mainloop
                        </span>
                      {/if}
                    </div>
                    <p class="text-term-fg mt-1 text-sm">{pr.title}</p>
                    <p class="text-term-fg-muted mt-1 text-xs">
                      by {pr.author} · {formatDate(pr.created_at)}
                    </p>
                  </div>
                </div>
              </a>
            {/each}
          </div>
        {:else}
          <p class="text-term-fg-muted text-sm">No open pull requests</p>
        {/if}
      </section>

      <section class="mb-6" aria-labelledby="workspaces-heading">
        <div class="mb-3 flex flex-wrap items-center justify-between gap-3">
          <h2 id="workspaces-heading" class="text-term-fg text-sm font-semibold">
            Branch workspaces
          </h2>
          <form
            class="flex flex-wrap items-end gap-2"
            onsubmit={(event) => {
              event.preventDefault();
              void createWorkspace(project);
            }}
          >
            <label class="text-term-fg-muted text-xs">
              Branch
              <input
                aria-label="Workspace branch"
                class="border-term-border bg-term-bg text-term-fg mt-1 block border px-2 py-1.5 text-sm"
                value={branch || project.default_branch}
                oninput={(event) => {
                  branch = event.currentTarget.value;
                }}
                disabled={workspaceBusy}
              />
            </label>
            <button
              type="submit"
              class="border-term-accent text-term-accent hover:bg-term-accent hover:text-term-bg border px-3 py-1.5 text-sm disabled:opacity-50"
              disabled={workspaceBusy}
            >
              {workspaceBusy ? 'Creating…' : 'New workspace'}
            </button>
          </form>
        </div>
        {#if workspaceError}
          <p class="text-term-red mb-3 text-sm" role="alert">{workspaceError}</p>
        {/if}
        {#if projectWorkspaces.length}
          <div class="space-y-2">
            {#each projectWorkspaces as workspace (workspace.workspace_id)}
              <a
                href="/workspaces/{workspace.workspace_id}"
                class="border-term-border bg-term-bg hover:border-term-accent flex items-center justify-between gap-3 border p-3"
              >
                <span class="min-w-0">
                  <span class="text-term-fg block truncate font-mono text-sm"
                    >{workspace.manifest.branch}</span
                  >
                  <span class="text-term-fg-muted mt-1 block text-xs">
                    {workspace.last_activity_at
                      ? `Active ${formatDate(workspace.last_activity_at)}`
                      : 'No activity recorded'}
                  </span>
                </span>
                <span class="text-term-cyan shrink-0 text-xs">{workspace.observed_state}</span>
              </a>
            {/each}
          </div>
        {:else}
          <p class="text-term-fg-muted text-sm">No branch workspaces yet.</p>
        {/if}
      </section>

      <!-- Recent Commits -->
      <section class="mb-6">
        <h2 class="text-term-fg mb-3 text-sm font-semibold">Recent Commits</h2>
        {#if recent_commits.length > 0}
          <div class="space-y-2">
            {#each recent_commits as commit (commit.sha)}
              <a
                href={commit.url}
                target="_blank"
                rel="noopener noreferrer"
                class="border-term-border bg-term-bg hover:border-term-accent block border p-3"
              >
                <p class="text-term-fg text-sm">{commit.message.split('\n')[0]}</p>
                <div class="text-term-fg-muted mt-1 flex items-center gap-2 text-xs">
                  <span>{commit.author}</span>
                  <span>·</span>
                  <span>{formatDate(commit.date)}</span>
                  <span>·</span>
                  <code class="font-mono">{commit.sha.slice(0, 7)}</code>
                </div>
              </a>
            {/each}
          </div>
        {:else}
          <p class="text-term-fg-muted text-sm">No recent commits</p>
        {/if}
      </section>

      <!-- Sessions working on this project -->
      <section>
        <h2 class="text-term-fg mb-3 text-sm font-semibold">Sessions</h2>
        {#if projectSessions.length > 0}
          <div class="space-y-2">
            {#each projectSessions as session (session.id)}
              <a
                href="/sessions/{session.id}"
                class="border-term-border bg-term-bg hover:border-term-accent block border p-3"
              >
                <div class="flex items-start justify-between gap-2">
                  <div class="min-w-0 flex-1">
                    <p class="text-term-fg truncate text-sm">
                      {session.title || 'Untitled session'}
                    </p>
                    <p class="text-term-fg-muted mt-1 text-xs">
                      {formatDate(session.created_at)}
                    </p>
                  </div>
                  <span class="shrink-0 text-xs {getStatusColor(session.status)}">
                    {statusLabel(session.status)}
                  </span>
                </div>
              </a>
            {/each}
          </div>
        {:else}
          <p class="text-term-fg-muted text-sm">No sessions yet</p>
        {/if}
      </section>
    </div>
  {:else}
    <div class="flex h-full items-center justify-center">
      <p class="text-term-fg-muted text-sm">Loading project...</p>
    </div>
  {/if}
</div>
