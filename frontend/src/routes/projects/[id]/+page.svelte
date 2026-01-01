<script lang="ts">
  import { onMount } from 'svelte';
  import { page } from '$app/stores';
  import { projects, currentProject } from '$lib/stores/projects';
  import { goto } from '$app/navigation';

  const projectId = $page.params.id;

  onMount(() => {
    projects.fetchProjectDetail(projectId);
  });

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
        return 'text-green-400';
      case 'failed':
        return 'text-red-400';
      case 'in_progress':
        return 'text-yellow-400';
      default:
        return 'text-term-fg-muted';
    }
  }
</script>

<svelte:head>
  <title>{$currentProject?.project.full_name || 'Project'} - mainloop</title>
</svelte:head>

<div class="flex h-full flex-col overflow-hidden bg-term-bg">
  {#if $currentProject}
    {@const { project, open_prs, recent_commits, tasks } = $currentProject}

    <!-- Header -->
    <header class="border-b border-term-border px-6 py-4">
      <button
        onclick={() => goto('/')}
        class="mb-3 flex items-center gap-1 text-xs text-term-fg-muted hover:text-term-accent"
      >
        <svg class="h-3 w-3" fill="none" viewBox="0 0 24 24" stroke="currentColor">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 19l-7-7 7-7" />
        </svg>
        Back
      </button>

      <div class="flex items-start gap-3">
        {#if project.avatar_url}
          <img src={project.avatar_url} alt="" class="h-12 w-12 rounded" />
        {/if}
        <div class="flex-1">
          <h1 class="text-2xl font-bold text-term-accent">{project.full_name}</h1>
          {#if project.description}
            <p class="mt-1 text-sm text-term-fg-muted">{project.description}</p>
          {/if}
          <div class="mt-2 flex items-center gap-3">
            <a
              href={project.html_url}
              target="_blank"
              rel="noopener noreferrer"
              class="text-xs text-term-fg hover:text-term-accent"
            >
              View on GitHub →
            </a>
            <span class="text-xs text-term-fg-muted">
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
        <h2 class="mb-3 text-sm font-semibold text-term-fg">Open Pull Requests</h2>
        {#if open_prs.length > 0}
          <div class="space-y-2">
            {#each open_prs as pr (pr.number)}
              <a
                href={pr.url}
                target="_blank"
                rel="noopener noreferrer"
                class="block border border-term-border bg-term-bg p-3 hover:border-term-accent"
              >
                <div class="flex items-start justify-between gap-2">
                  <div class="flex-1">
                    <div class="flex items-center gap-2">
                      <span class="text-sm font-medium text-term-fg">#{pr.number}</span>
                      {#if pr.is_mainloop}
                        <span class="rounded bg-term-accent px-1.5 py-0.5 text-xs text-term-bg">
                          mainloop
                        </span>
                      {/if}
                    </div>
                    <p class="mt-1 text-sm text-term-fg">{pr.title}</p>
                    <p class="mt-1 text-xs text-term-fg-muted">
                      by {pr.author} · {formatDate(pr.created_at)}
                    </p>
                  </div>
                </div>
              </a>
            {/each}
          </div>
        {:else}
          <p class="text-sm text-term-fg-muted">No open pull requests</p>
        {/if}
      </section>

      <!-- Recent Commits -->
      <section class="mb-6">
        <h2 class="mb-3 text-sm font-semibold text-term-fg">Recent Commits</h2>
        {#if recent_commits.length > 0}
          <div class="space-y-2">
            {#each recent_commits as commit (commit.sha)}
              <a
                href={commit.url}
                target="_blank"
                rel="noopener noreferrer"
                class="block border border-term-border bg-term-bg p-3 hover:border-term-accent"
              >
                <p class="text-sm text-term-fg">{commit.message.split('\n')[0]}</p>
                <div class="mt-1 flex items-center gap-2 text-xs text-term-fg-muted">
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
          <p class="text-sm text-term-fg-muted">No recent commits</p>
        {/if}
      </section>

      <!-- Associated Tasks -->
      <section>
        <h2 class="mb-3 text-sm font-semibold text-term-fg">Tasks</h2>
        {#if tasks.length > 0}
          <div class="space-y-2">
            {#each tasks as task (task.id)}
              <div class="border border-term-border bg-term-bg p-3">
                <div class="flex items-start justify-between gap-2">
                  <div class="flex-1">
                    <p class="text-sm text-term-fg">{task.title || 'Untitled task'}</p>
                    <p class="mt-1 text-xs text-term-fg-muted">
                      {formatDate(task.created_at)}
                    </p>
                  </div>
                  <span class="text-xs {getStatusColor(task.status)}">
                    {task.status}
                  </span>
                </div>
              </div>
            {/each}
          </div>
        {:else}
          <p class="text-sm text-term-fg-muted">No tasks yet</p>
        {/if}
      </section>
    </div>
  {:else}
    <div class="flex h-full items-center justify-center">
      <p class="text-sm text-term-fg-muted">Loading project...</p>
    </div>
  {/if}
</div>
