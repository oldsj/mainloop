<script lang="ts">
  import { page } from '$app/stores';
</script>

<div class="flex h-full items-center justify-center">
  <div class="text-center">
    <h1 class="text-4xl font-bold text-neutral-900">{$page.status}</h1>
    <p class="mt-2 text-neutral-600">{$page.error?.message || 'An error occurred'}</p>
  </div>
</div>
