<script lang="ts">
  import '../app.css';
  import type { LayoutData } from './$types';
  import { onMount } from 'svelte';
  import { inbox } from '$lib/stores/inbox';
  import { tasks } from '$lib/stores/tasks';
  import { themeStore } from '$lib/stores/theme';
  import { mobileTab, type MobileTab } from '$lib/stores/mobileTab';
  import { connectSSE, disconnectSSE } from '$lib/sse';
  import TasksBadge from '$lib/components/TasksBadge.svelte';
  import TasksPanel from '$lib/components/TasksPanel.svelte';
  import ProjectList from '$lib/components/ProjectList.svelte';
  import MobileTabBar from '$lib/components/MobileTabBar.svelte';
  import ThemeSelector from '$lib/components/ThemeSelector.svelte';
  import { beforeNavigate } from '$app/navigation';

  let { children, data }: { children: any; data: LayoutData } = $props();

  // Local state that syncs with store for reliable reactivity
  let activeTab = $state<MobileTab>('chat');

  $effect(() => {
    const unsubscribe = mobileTab.subscribe(value => {
      activeTab = value;
    });
    return unsubscribe;
  });

  // Reset mobile tab to chat on navigation
  beforeNavigate(() => {
    mobileTab.set('chat');
  });

  onMount(() => {
    themeStore.initialize();

    // Connect SSE for real-time updates
    connectSSE();

    // Start listening for SSE events
    inbox.startListening();
    tasks.startListening();

    return () => {
      inbox.stopListening();
      tasks.stopListening();
      disconnectSSE();
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
    </div>
  </header>

  <div class="flex flex-1 overflow-hidden">
    <main class="flex-1 overflow-hidden">
      {@render children()}
    </main>

    <!-- Desktop: Always visible side panels -->
    <div class="flex w-full max-w-md flex-col border-l border-term-border bg-term-bg">
      <div class="flex-1 overflow-hidden">
        <TasksPanel desktop={true} />
      </div>
      <ProjectList />
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
    {/if}
  </div>

  <MobileTabBar />
</div>
