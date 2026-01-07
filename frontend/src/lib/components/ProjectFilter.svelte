<script lang="ts">
  import { projects, projectsList, selectedProjectId } from '$lib/stores/projects';

  let isOpen = $state(false);

  function handleSelect(projectId: string | null) {
    projects.selectProject(projectId);
    isOpen = false;
  }
</script>

<div class="relative">
  <button
    type="button"
    onclick={() => (isOpen = !isOpen)}
    class="flex items-center gap-1 border border-term-border px-2 py-1 text-xs text-term-fg hover:border-term-accent"
  >
    {#if $selectedProjectId}
      {@const project = $projectsList.find((p) => p.id === $selectedProjectId)}
      <span class="max-w-24 truncate">{project?.name || 'Filter'}</span>
    {:else}
      All Projects
    {/if}
    <svg class="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke="currentColor">
      <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7" />
    </svg>
  </button>

  {#if isOpen}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <!-- svelte-ignore a11y_click_events_have_key_events -->
    <div
      class="fixed inset-0 z-10"
      onclick={() => (isOpen = false)}
    ></div>
    <div
      class="absolute right-0 top-full z-10 mt-1 min-w-40 border border-term-border bg-term-bg shadow-lg"
    >
      <button
        type="button"
        onclick={() => handleSelect(null)}
        class="w-full px-3 py-2 text-left text-sm text-term-fg hover:bg-term-selection"
      >
        All Projects
      </button>
      {#each $projectsList as project (project.id)}
        <button
          type="button"
          onclick={() => handleSelect(project.id)}
          class="w-full px-3 py-2 text-left text-sm hover:bg-term-selection
                 {$selectedProjectId === project.id ? 'text-term-accent' : 'text-term-fg'}"
        >
          {project.full_name}
        </button>
      {/each}
    </div>
  {/if}
</div>
