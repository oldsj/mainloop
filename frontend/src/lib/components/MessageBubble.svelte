<script lang="ts">
  import type { Message } from '$lib/api';

  let { message }: { message: Message } = $props();
  let isUser = $derived(message.role === 'user');
</script>

<div class="flex {isUser ? 'justify-end' : 'justify-start'}">
  <div
    class="max-w-[80%] rounded-lg px-4 py-2 {isUser
      ? 'bg-primary-500 text-white'
      : 'bg-neutral-200 text-neutral-900'}"
  >
    <p class="whitespace-pre-wrap break-words">{message.content}</p>
    <time class="mt-1 block text-xs opacity-70">
      {new Date(message.created_at).toLocaleTimeString()}
    </time>
  </div>
</div>
