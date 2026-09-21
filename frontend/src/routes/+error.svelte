<script lang="ts">
  import { page } from '$app/stores';
</script>

<div class="flex h-full items-center justify-center bg-term-bg">
  <div class="text-center">
    <h1 class="text-4xl font-bold text-term-fg">{$page.status}</h1>
    <p class="mt-2 text-term-fg-muted">{$page.error?.message || 'An error occurred'}</p>
    <a href="/" class="mt-4 inline-block text-term-accent hover:underline">Back to home</a>
  </div>
</div>
