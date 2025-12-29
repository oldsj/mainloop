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
  import MobileTabBar from '$lib/components/MobileTabBar.svelte';

  let { children, data }: { children: any; data: LayoutData } = $props();

  type Tab = 'chat' | 'tasks' | 'inbox';
  let activeTab = $state<Tab>('chat');

  onMount(() => {
    inbox.startPolling(30000);
    tasks.startPolling(10000); // Poll tasks more frequently
    return () => {
      inbox.stopPolling();
      tasks.stopPolling();
    };
  });
</script>

<!-- Desktop Layout (≥768px) -->
<div class="hidden h-screen flex-col md:flex">
  <header class="flex items-center justify-between border-b border-neutral-200 bg-white px-4 py-3">
    <h1 class="text-xl font-semibold text-neutral-900">mainloop</h1>
    <div class="flex items-center gap-1">
      <TasksBadge />
      <InboxBadge />
    </div>
  </header>

  <div class="flex flex-1 overflow-hidden">
    <main class="flex-1 overflow-hidden">
      {@render children()}
    </main>

    <!-- Desktop: Always visible side panels -->
    <div class="w-full max-w-md border-l border-neutral-200 bg-white">
      <TasksPanel desktop={true} />
    </div>
  </div>
</div>

<!-- Mobile Layout (<768px) -->
<div class="flex h-screen flex-col md:hidden">
  <header class="flex items-center justify-between border-b border-neutral-200 bg-white px-4 py-3">
    <h1 class="text-xl font-semibold text-neutral-900">mainloop</h1>
  </header>

  <div class="flex-1 overflow-hidden pb-16">
    {#if activeTab === 'chat'}
      <div class="h-full overflow-hidden">
        {@render children()}
      </div>
    {:else if activeTab === 'tasks'}
      <TasksPanel desktop={false} mobile={true} />
    {:else if activeTab === 'inbox'}
      <InboxPanel desktop={false} mobile={true} />
    {/if}
  </div>

  <MobileTabBar bind:activeTab />
</div>

<!-- Desktop overlays for inbox -->
<div class="hidden md:block">
  <InboxPanel desktop={true} />
</div>
