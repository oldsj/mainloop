<script lang="ts">
  import '../app.css';
  import type { LayoutData } from './$types';

  let { children, data }: { children: any; data: LayoutData } = $props();
</script>

<div class="flex h-screen flex-col">
  <header class="border-b border-neutral-200 bg-white px-4 py-3">
    <h1 class="text-xl font-semibold text-neutral-900">mainloop</h1>
  </header>

  <main class="flex-1 overflow-hidden">
    {@render children()}
  </main>
</div>
