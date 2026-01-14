<script lang="ts">
  import type { Session } from '$lib/api';

  let {
    session,
    onclick
  }: {
    session: Session;
    onclick?: () => void;
  } = $props();

  const statusColors: Record<string, string> = {
    pending: 'text-term-yellow',
    active: 'text-term-cyan',
    waiting_on_user: 'text-term-magenta',
    waiting_questions: 'text-term-magenta',
    waiting_plan_review: 'text-term-magenta',
    ready_to_implement: 'text-term-yellow',
    planning: 'text-term-cyan',
    implementing: 'text-term-cyan',
    under_review: 'text-term-yellow',
    completed: 'text-term-green',
    failed: 'text-term-red',
    cancelled: 'text-term-fg-muted'
  };

  const statusLabels: Record<string, string> = {
    pending: 'PENDING',
    active: 'ACTIVE',
    waiting_on_user: 'NEEDS INPUT',
    waiting_questions: 'NEEDS INPUT',
    waiting_plan_review: 'REVIEW PLAN',
    ready_to_implement: 'READY',
    planning: 'PLANNING',
    implementing: 'IMPLEMENTING',
    under_review: 'IN REVIEW',
    completed: 'DONE',
    failed: 'FAILED',
    cancelled: 'CANCELLED'
  };

  // Check if session needs user attention
  const needsAttention = $derived(
    ['waiting_on_user', 'waiting_questions', 'waiting_plan_review'].includes(session.status)
  );

  // Check if session is actively running
  const isActive = $derived(
    ['active', 'planning', 'implementing'].includes(session.status)
  );

  // Extract repo name from URL
  function getRepoName(repoUrl: string): string {
    return repoUrl.replace('https://github.com/', '');
  }

  function formatTime(dateStr: string): string {
    const date = new Date(dateStr);
    const now = new Date();
    const diff = now.getTime() - date.getTime();
    const minutes = Math.floor(diff / 60000);
    const hours = Math.floor(minutes / 60);
    const days = Math.floor(hours / 24);

    if (days > 0) return `${days}d ago`;
    if (hours > 0) return `${hours}h ago`;
    if (minutes > 0) return `${minutes}m ago`;
    return 'just now';
  }
</script>

<button
  type="button"
  class="w-full border border-l-4 border-term-border bg-term-bg-secondary p-3 text-left transition-colors hover:border-term-accent {needsAttention ? 'bg-term-magenta/5' : ''}"
  style="border-left-color: {session.color};"
  {onclick}
>
  <div class="flex items-start justify-between gap-2">
    <div class="min-w-0 flex-1">
      <div class="flex items-center gap-2">
        {#if isActive}
          <span class="h-3 w-3 animate-spin rounded-full border border-term-cyan border-t-transparent"></span>
        {/if}
        <span class="text-xs {statusColors[session.status] || 'text-term-fg-muted'}">
          [{statusLabels[session.status] || session.status.toUpperCase()}]
        </span>
        <h3 class="truncate text-sm font-medium text-term-fg">
          {session.title}
        </h3>
      </div>

      <p class="mt-1 truncate text-xs text-term-fg-muted">
        {session.description}
      </p>

      {#if session.repo_url}
        <div class="mt-1 flex items-center gap-2 text-xs text-term-fg-muted">
          <span class="text-term-accent">in {getRepoName(session.repo_url)}</span>
          {#if session.issue_number}
            <a
              href={session.issue_url}
              target="_blank"
              rel="noopener noreferrer"
              class="hover:text-term-accent"
              onclick={(e) => e.stopPropagation()}
            >
              Issue #{session.issue_number}
            </a>
          {/if}
          {#if session.pr_number}
            <a
              href={session.pr_url}
              target="_blank"
              rel="noopener noreferrer"
              class="hover:text-term-accent"
              onclick={(e) => e.stopPropagation()}
            >
              PR #{session.pr_number}
            </a>
          {/if}
        </div>
      {/if}
    </div>
    <span class="shrink-0 text-xs text-term-fg-muted">
      {formatTime(session.created_at)}
    </span>
  </div>

  {#if needsAttention}
    <div class="mt-2 flex items-center gap-1 text-xs text-term-magenta">
      <span class="animate-pulse">*</span>
      <span>Waiting for your input</span>
    </div>
  {/if}

  {#if session.error}
    <div class="mt-2 truncate text-xs text-term-red">
      Error: {session.error}
    </div>
  {/if}
</button>
