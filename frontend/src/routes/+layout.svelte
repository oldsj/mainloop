<script lang="ts">
  import '../app.css';
  import type { LayoutData } from './$types';
  import { onMount } from 'svelte';
  import { inbox } from '$lib/stores/inbox';
  import { tasks } from '$lib/stores/tasks';
  import InboxBadge from '$lib/components/InboxBadge.svelte';
  import InboxPanel from '$lib/components/InboxPanel.svelte';
  import TasksBadge from '$lib/components/TasksBadge.svelte';
  import TasksPanel from '$lib/components/TasksPanel.svelte';

  let { children, data }: { children: any; data: LayoutData } = $props();

  onMount(() => {
    inbox.startPolling(30000);
    tasks.startPolling(10000); // Poll tasks more frequently
    return () => {
      inbox.stopPolling();
      tasks.stopPolling();
    };
  });
</script>

<div class="flex h-screen flex-col">
  <header class="flex items-center justify-between border-b border-neutral-200 bg-white px-4 py-3">
    <h1 class="text-xl font-semibold text-neutral-900">mainloop</h1>
    <div class="flex items-center gap-1">
      <TasksBadge />
      <InboxBadge />
    </div>
  </header>

  <main class="flex-1 overflow-hidden">
    {@render children()}
  </main>
</div>

<InboxPanel />
<TasksPanel />
