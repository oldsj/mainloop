<script lang="ts">
  import { page } from '$app/stores';
  import { tasks, taskViews } from '$lib/stores/tasks';
  import TaskDetail from '$lib/components/TaskDetail.svelte';

  const taskId = $derived($page.params.id ?? '');
  const view = $derived($tasks.byId[taskId]);
  const failure = $derived($tasks.taskErrors[taskId]);

  $effect(() => {
    const id = taskId;
    if (!id) return;
    const controller = new AbortController();
    // Children and siblings give the detail its tree; the task itself is the source of truth.
    void tasks.fetchTask(id, controller.signal).then(async () => {
      const loaded = $tasks.byId[id];
      if (!loaded) return;
      const parent = loaded.task.parent_task_id;
      if (parent && !$tasks.byId[parent]) await tasks.fetchTask(parent, controller.signal);
      await tasks.fetchList({ parent_task_id: id }, controller.signal);
    });
    return () => controller.abort();
  });
</script>

<svelte:head><title>{view ? view.task.title : 'Task'} · Mainloop</title></svelte:head>

<div class="h-full min-w-0 overflow-y-auto">
  <div class="mx-auto flex w-full max-w-3xl flex-col gap-4 p-4">
    <a href="/" class="text-term-fg-muted text-xs hover:underline">← Back</a>

    {#if view}
      {#if failure}
        <div class="border-term-yellow/50 text-term-yellow border px-3 py-2 text-sm" role="alert">
          <p>{failure.message} Showing the last known state.</p>
          <button
            type="button"
            onclick={() => void tasks.refresh(taskId)}
            class="border-term-border text-term-fg mt-2 min-h-11 border px-4 py-2"
          >
            Retry loading
          </button>
        </div>
      {/if}
      <TaskDetail {view} views={$taskViews} />
    {:else if failure}
      <div class="border-term-red/50 text-term-red border px-3 py-3 text-sm" role="alert">
        <p>{failure.message}</p>
        <button
          type="button"
          onclick={() => void tasks.fetchTask(taskId)}
          class="border-term-border text-term-fg mt-2 min-h-11 border px-4 py-2"
        >
          Retry loading
        </button>
      </div>
    {:else}
      <p class="text-term-fg-muted text-sm" role="status">Loading task…</p>
    {/if}
  </div>
</div>
