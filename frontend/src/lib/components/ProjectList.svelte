<script lang="ts">
  import { projects, projectsList } from '$lib/stores/projects';
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';

  onMount(() => {
    projects.fetchProjects();
  });

  function handleClick(projectId: string) {
    goto(`/projects/${projectId}`);
  }
</script>

<div class="border-b border-term-border">
  <div class="flex items-center justify-between px-4 py-2">
    <span class="text-xs text-term-fg-muted">PROJECTS</span>
  </div>

  <div class="max-h-48 overflow-y-auto" data-testid="projects-list">
    {#each $projectsList as project (project.id)}
      <button
        onclick={() => handleClick(project.id)}
        class="flex w-full items-center gap-2 truncate px-4 py-2 text-left text-sm text-term-fg hover:bg-term-selection"
      >
        {#if project.avatar_url}
          <img src={project.avatar_url} alt="" class="h-4 w-4 rounded" />
        {/if}
        <span class="truncate">{project.full_name}</span>
      </button>
    {/each}

    {#if $projectsList.length === 0}
      <p class="px-4 py-2 text-xs text-term-fg-muted">No projects yet</p>
    {/if}
  </div>
</div>
