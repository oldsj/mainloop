<script lang="ts">
  import { projects, projectsList } from '$lib/stores/projects';
  import { api } from '$lib/api';
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';

  let formOpen = $state(false);
  let repo = $state('');
  let branch = $state('');
  let busy = $state(false);
  let error = $state<string | null>(null);

  onMount(() => {
    projects.fetchProjects();
  });

  function handleClick(projectId: string) {
    goto(`/projects/${projectId}`);
  }

  async function createWorkspace() {
    if (!repo.trim()) {
      error = 'Enter a repository';
      return;
    }
    busy = true;
    error = null;
    try {
      const workspace = await api.createWorkspaceFromRepo(repo.trim(), branch.trim());
      void projects.fetchProjects();
      repo = '';
      branch = '';
      formOpen = false;
      await goto(`/workspaces/${workspace.workspace_id}`);
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed to create workspace';
    } finally {
      busy = false;
    }
  }
</script>

<div class="border-term-border border-b">
  <div class="flex items-center justify-between px-4 py-2">
    <span class="text-term-fg-muted text-xs">PROJECTS</span>
    <button
      type="button"
      onclick={() => (formOpen = !formOpen)}
      aria-expanded={formOpen}
      class="text-term-accent text-xs hover:underline"
    >
      {formOpen ? 'Cancel' : 'New workspace'}
    </button>
  </div>

  {#if formOpen}
    <form
      class="space-y-2 px-4 pb-3"
      data-testid="new-workspace-form"
      onsubmit={(event) => {
        event.preventDefault();
        void createWorkspace();
      }}
    >
      <input
        aria-label="Repository"
        placeholder="owner/name or https://github.com/owner/name"
        class="border-term-border bg-term-bg text-term-fg block w-full border px-2 py-1.5 text-sm"
        bind:value={repo}
        disabled={busy}
        autocapitalize="off"
        autocomplete="off"
        spellcheck="false"
      />
      <input
        aria-label="Branch (optional)"
        placeholder="branch (optional)"
        class="border-term-border bg-term-bg text-term-fg block w-full border px-2 py-1.5 text-sm"
        bind:value={branch}
        disabled={busy}
        autocapitalize="off"
        autocomplete="off"
        spellcheck="false"
      />
      <button
        type="submit"
        class="border-term-accent text-term-accent hover:bg-term-accent hover:text-term-bg border px-3 py-1.5 text-sm disabled:opacity-50"
        disabled={busy}
      >
        {busy ? 'Creating…' : 'Create workspace'}
      </button>
      {#if error}
        <p class="text-term-red text-sm" role="alert">{error}</p>
      {/if}
    </form>
  {/if}

  <div class="max-h-48 overflow-y-auto" data-testid="projects-list">
    {#each $projectsList as project (project.id)}
      <button
        type="button"
        onclick={() => handleClick(project.id)}
        class="text-term-fg hover:bg-term-selection flex w-full items-center gap-2 truncate px-4 py-2 text-left text-sm"
      >
        {#if project.avatar_url}
          <img src={project.avatar_url} alt="" class="h-4 w-4 rounded" />
        {/if}
        <span class="truncate">{project.full_name}</span>
      </button>
    {/each}

    {#if $projectsList.length === 0}
      <p class="text-term-fg-muted px-4 py-2 text-xs">No projects yet</p>
    {/if}
  </div>
</div>
