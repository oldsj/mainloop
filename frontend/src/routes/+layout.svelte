<script lang="ts">
  import '../app.css';
  import type { LayoutData } from './$types';
  import { onMount } from 'svelte';
  import { inbox } from '$lib/stores/inbox';
  import InboxBadge from '$lib/components/InboxBadge.svelte';
  import InboxPanel from '$lib/components/InboxPanel.svelte';

  let { children, data }: { children: any; data: LayoutData } = $props();

  onMount(() => {
    inbox.startPolling(30000);
    return () => inbox.stopPolling();
  });
</script>

<div class="flex h-screen flex-col">
  <header class="flex items-center justify-between border-b border-neutral-200 bg-white px-4 py-3">
    <h1 class="text-xl font-semibold text-neutral-900">mainloop</h1>
    <InboxBadge />
  </header>

  <main class="flex-1 overflow-hidden">
    {@render children()}
  </main>
</div>

<InboxPanel />
