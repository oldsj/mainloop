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
    implementing: 'IMPLEMENTING',
    under_review: 'IN REVIEW',
    completed: 'DONE',
    failed: 'FAILED',
    cancelled: 'CANCELLED'
  };

  // Check if session needs user attention
  const needsAttention = $derived(session.status === 'waiting_on_user');

  // Check if session is actively running
  const isActive = $derived(['active', 'implementing'].includes(session.status));

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
  <!-- Row 1: Title + time -->
  <div class="flex items-center justify-between gap-2">
    <div class="flex min-w-0 items-center gap-2">
      {#if isActive}
        <span class="h-3 w-3 animate-spin rounded-full border border-term-cyan border-t-transparent"></span>
      {/if}
      <h3 class="truncate text-sm font-medium text-term-fg">
        {session.title}
      </h3>
    </div>
    <span class="shrink-0 text-xs text-term-fg-muted">{formatTime(session.created_at)}</span>
  </div>

  <!-- Row 2: Label + links (left), repo (right) -->
  <div class="mt-2 flex items-center justify-between gap-2">
    <div class="flex items-center gap-2">
      <span
        class="whitespace-nowrap px-2 py-0.5 text-xs font-medium {needsAttention
          ? 'bg-term-magenta/20 text-term-magenta'
          : session.status === 'completed'
            ? 'bg-term-green/20 text-term-green'
            : session.status === 'failed' || session.status === 'cancelled'
              ? 'bg-term-red/20 text-term-red'
              : isActive
                ? 'bg-term-cyan/20 text-term-cyan'
                : 'bg-term-fg-muted/20 text-term-fg-muted'}"
        style="border-radius: 9999px;"
      >
        {statusLabels[session.status] || session.status.toUpperCase()}
      </span>
      {#if session.issue_number}
        <a
          href={session.issue_url}
          target="_blank"
          rel="noopener noreferrer"
          class="text-xs text-term-fg-muted hover:text-term-accent"
          onclick={(e) => e.stopPropagation()}
        >
          #{session.issue_number}
        </a>
      {/if}
      {#if session.pr_number}
        <a
          href={session.pr_url}
          target="_blank"
          rel="noopener noreferrer"
          class="text-xs text-term-fg-muted hover:text-term-accent"
          onclick={(e) => e.stopPropagation()}
        >
          PR #{session.pr_number}
        </a>
      {/if}
    </div>
    {#if session.repo_url}
      <span class="text-xs text-term-accent">{getRepoName(session.repo_url)}</span>
    {/if}
  </div>

  {#if session.error}
    <div class="mt-2 truncate text-xs text-term-red">
      Error: {session.error}
    </div>
  {/if}
</button>
