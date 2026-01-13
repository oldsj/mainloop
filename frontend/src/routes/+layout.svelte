<script lang="ts">
  import '../app.css';
  import type { LayoutData } from './$types';
  import { onMount } from 'svelte';
  import { inbox } from '$lib/stores/inbox';
  import { sessions } from '$lib/stores/sessions';
  import { notifications } from '$lib/stores/notifications';
  import { themeStore } from '$lib/stores/theme';
  import { mobileTab } from '$lib/stores/mobileTab';
  import { isMobile } from '$lib/stores/viewport';
  import { connectSSE, disconnectSSE, getSSEClient } from '$lib/sse';
  import TasksBadge from '$lib/components/TasksBadge.svelte';
  import TasksPanel from '$lib/components/TasksPanel.svelte';
  import SessionList from '$lib/components/SessionList.svelte';
  import ProjectList from '$lib/components/ProjectList.svelte';
  import MobileTabBar from '$lib/components/MobileTabBar.svelte';
  import ThemeSelector from '$lib/components/ThemeSelector.svelte';
  import NotificationToast from '$lib/components/NotificationToast.svelte';
  import { beforeNavigate } from '$app/navigation';

  let { children, data }: { children: any; data: LayoutData } = $props();

  // Derive activeTab directly from store for reliable reactivity
  let activeTab = $derived($mobileTab);

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

    // Listen for session events
    const client = getSSEClient();
    const unsubSessionUpdated = client.on('session:updated', (event) => {
      const { session_id, status } = event.data as { session_id: string; status: string };
      sessions.updateSession(session_id, { status: status as any });
    });
    const unsubSessionNeedsInput = client.on('session:needs_input', (event) => {
      const { session_id, title, preview } = event.data as {
        session_id: string;
        title: string;
        preview: string;
      };
      notifications.addNotification({
        id: `notif-${Date.now()}`,
        session_id,
        user_id: '',
        title,
        preview,
        read: false,
        created_at: new Date().toISOString()
      });
    });

    // Fetch initial data
    sessions.fetchSessions();
    notifications.fetchNotifications();

    return () => {
      inbox.stopListening();
      unsubSessionUpdated();
      unsubSessionNeedsInput();
      disconnectSSE();
    };
  });
</script>

<!-- Notification toasts -->
<NotificationToast />

{#if $isMobile}
  <!-- Mobile Layout -->
  <div class="flex h-screen flex-col">
    <header class="flex items-center justify-between border-b border-term-border bg-term-bg px-4 py-3">
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
{:else}
  <!-- Desktop Layout -->
  <div class="flex h-screen flex-col">
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
        <div class="h-1/3 overflow-hidden border-b border-term-border">
          <SessionList />
        </div>
        <div class="h-1/3 overflow-hidden border-b border-term-border">
          <TasksPanel desktop={true} />
        </div>
        <div class="h-1/3 overflow-hidden">
          <ProjectList />
        </div>
      </div>
    </div>
  </div>
{/if}
