<script lang="ts">
  import '../app.css';
  import type { LayoutData } from './$types';
  import { onMount } from 'svelte';
  import { inbox } from '$lib/stores/inbox';
  import { tasks } from '$lib/stores/tasks';
  import { themeStore } from '$lib/stores/theme';
  import InboxBadge from '$lib/components/InboxBadge.svelte';
  import InboxPanel from '$lib/components/InboxPanel.svelte';
  import TasksBadge from '$lib/components/TasksBadge.svelte';
  import TasksPanel from '$lib/components/TasksPanel.svelte';
  import MobileTabBar from '$lib/components/MobileTabBar.svelte';
  import ThemeSelector from '$lib/components/ThemeSelector.svelte';

  let { children, data }: { children: any; data: LayoutData } = $props();

  type Tab = 'chat' | 'tasks' | 'inbox';
  let activeTab = $state<Tab>('chat');

  onMount(() => {
    themeStore.initialize();
    inbox.startPolling(30000);
    tasks.startPolling(10000);
    return () => {
      inbox.stopPolling();
      tasks.stopPolling();
    };
  });
</script>

<!-- Desktop Layout (>=768px) -->
<div class="hidden h-screen flex-col md:flex">
  <header class="flex items-center justify-between border-b border-term-border bg-term-bg px-4 py-3">
    <h1 class="text-xl text-term-accent">
      <span class="text-term-fg-muted">$</span> mainloop
    </h1>
    <div class="flex items-center gap-3">
      <ThemeSelector />
      <TasksBadge />
      <InboxBadge />
    </div>
  </header>

  <div class="flex flex-1 overflow-hidden">
    <main class="flex-1 overflow-hidden">
      {@render children()}
    </main>

    <!-- Desktop: Always visible side panels -->
    <div class="w-full max-w-md border-l border-term-border bg-term-bg">
      <TasksPanel desktop={true} />
    </div>
  </div>
</div>

<!-- Mobile Layout (<768px) -->
<div class="flex h-screen flex-col md:hidden">
  <header
    class="flex items-center justify-between border-b border-term-border bg-term-bg px-4 py-3"
  >
    <h1 class="text-xl text-term-accent">
      <span class="text-term-fg-muted">$</span> mainloop
    </h1>
    <ThemeSelector />
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
