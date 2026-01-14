<script lang="ts">
  import { notifications } from '$lib/stores/notifications';
  import { goto } from '$app/navigation';

  function handleNotificationClick(notificationId: string, sessionId: string) {
    notifications.dismissNotification(notificationId);
    goto(`/sessions/${sessionId}`);
  }

  function handleDismiss(event: Event, notificationId: string) {
    event.stopPropagation();
    notifications.dismissNotification(notificationId);
  }

  function handleKeydown(event: KeyboardEvent, notificationId: string, sessionId: string) {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      handleNotificationClick(notificationId, sessionId);
    }
  }
</script>

{#if $notifications.notifications.length > 0}
  <div class="fixed bottom-4 right-4 z-50 flex flex-col gap-2">
    {#each $notifications.notifications.slice(0, 3) as notification (notification.id)}
      <div
        role="button"
        tabindex="0"
        onclick={() => handleNotificationClick(notification.id, notification.session_id)}
        onkeydown={(e) => handleKeydown(e, notification.id, notification.session_id)}
        class="group flex w-80 cursor-pointer items-start gap-3 border border-term-border bg-term-bg p-3 shadow-lg transition-colors hover:border-term-accent"
      >
        <div class="flex h-6 w-6 shrink-0 items-center justify-center text-term-magenta">
          <svg xmlns="http://www.w3.org/2000/svg" class="h-5 w-5" viewBox="0 0 20 20" fill="currentColor">
            <path d="M10 2a6 6 0 00-6 6v3.586l-.707.707A1 1 0 004 14h12a1 1 0 00.707-1.707L16 11.586V8a6 6 0 00-6-6zM10 18a3 3 0 01-3-3h6a3 3 0 01-3 3z" />
          </svg>
        </div>
        <div class="min-w-0 flex-1 text-left">
          <p class="text-sm font-medium text-term-fg">{notification.title}</p>
          <p class="mt-1 truncate text-xs text-term-fg-muted">{notification.preview}</p>
        </div>
        <button
          type="button"
          onclick={(e) => handleDismiss(e, notification.id)}
          class="shrink-0 text-term-fg-muted opacity-0 transition-opacity hover:text-term-fg group-hover:opacity-100"
          aria-label="Dismiss"
        >
          <svg xmlns="http://www.w3.org/2000/svg" class="h-4 w-4" viewBox="0 0 20 20" fill="currentColor">
            <path fill-rule="evenodd" d="M4.293 4.293a1 1 0 011.414 0L10 8.586l4.293-4.293a1 1 0 111.414 1.414L11.414 10l4.293 4.293a1 1 0 01-1.414 1.414L10 11.414l-4.293 4.293a1 1 0 01-1.414-1.414L8.586 10 4.293 5.707a1 1 0 010-1.414z" clip-rule="evenodd" />
          </svg>
        </button>
      </div>
    {/each}
  </div>
{/if}
