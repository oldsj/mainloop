<script lang="ts">
  import type { TaskActionKind, TaskView } from '$lib/api';
  import { tasks } from '$lib/stores/tasks';
  import { actionAvailability, reassignTargets } from '$lib/taskActions';
  import { currentAttempt, switchBlockers } from '$lib/taskState';

  let { view }: { view: TaskView } = $props();

  const taskId = $derived(view.task.id);
  const actionState = $derived($tasks.actions[taskId]);
  const busy = $derived(actionState?.busy ?? false);
  const attempt = $derived(currentAttempt(view));
  const targets = $derived(reassignTargets($tasks.profiles, view, attempt));
  const blockers = $derived(switchBlockers(view));
  let target = $state('');
  let confirmCancel = $state(false);

  // Keep the choice valid as profiles load or the task's current provider changes.
  $effect(() => {
    if (!targets.some((profile) => profile.id === target)) target = targets[0]?.id ?? '';
  });

  $effect(() => {
    void tasks.loadProfiles();
  });

  const retry = $derived(actionAvailability(view, 'retry', { busy }));
  const reassign = $derived(actionAvailability(view, 'reassign', { busy }));
  const cancel = $derived(actionAvailability(view, 'cancel', { busy }));
  const reassignOff = $derived(
    reassign.enabled && targets.length === 0
      ? ($tasks.profilesError ?? 'No other qualified provider is available.')
      : reassign.reason
  );

  function run(kind: TaskActionKind) {
    void tasks.act(kind, taskId, { targetProfileId: kind === 'reassign' ? target : undefined });
  }
</script>

<section aria-labelledby="task-actions-heading" data-testid="task-actions">
  <h2 id="task-actions-heading" class="text-term-fg mb-2 text-sm">[ACTIONS]</h2>
  <p class="text-term-fg-muted mb-3 text-xs">
    Acting on version {view.task.version}{attempt ? `, attempt ${attempt.number}` : ', no attempt yet'}.
    If the task changes first, the server refuses and this page refreshes.
  </p>

  {#if actionState?.pending}
    <div class="border-term-yellow/50 mb-3 border px-3 py-2" data-testid="task-resend">
      <p class="text-term-fg-muted text-xs">
        Original {actionState.pending.kind}: version {actionState.pending.body.expected_version},
        attempt {actionState.pending.body.expected_attempt_id ?? 'none'},
        target {actionState.pending.body.target_profile_id ?? 'none'}.
      </p>
      <button type="button" disabled={busy} onclick={() => tasks.resend(taskId)}
        class="border-term-border text-term-fg mt-2 min-h-11 border px-4 py-2 text-sm disabled:opacity-50">
        Resend original request
      </button>
    </div>
  {/if}

  <div class="flex flex-col gap-3 sm:flex-row sm:flex-wrap sm:items-start">
    <div class="flex flex-col gap-1">
      <button
        type="button"
        onclick={() => run('retry')}
        disabled={!retry.enabled}
        aria-describedby="retry-why"
        class="border-term-border text-term-fg hover:border-term-accent hover:text-term-accent min-h-11 border px-4 py-2 text-sm disabled:opacity-50"
      >
        Retry
      </button>
      <span id="retry-why" class="text-term-fg-muted text-xs">{retry.reason ?? ''}</span>
    </div>

    <div class="flex flex-col gap-1">
      <div class="flex flex-wrap items-center gap-2">
        <label class="sr-only" for="reassign-target">Provider to reassign to</label>
        <select
          id="reassign-target"
          bind:value={target}
          disabled={!reassign.enabled || targets.length === 0}
          class="border-term-border bg-term-bg text-term-fg min-h-11 border px-2 text-sm disabled:opacity-50"
        >
          {#each targets as profile (profile.id)}
            <option value={profile.id}>{profile.display_name} ({profile.native_provider})</option>
          {/each}
        </select>
        <button
          type="button"
          onclick={() => run('reassign')}
          disabled={!reassign.enabled || targets.length === 0 || !target}
          aria-describedby="reassign-why"
          class="border-term-border text-term-fg hover:border-term-accent hover:text-term-accent min-h-11 border px-4 py-2 text-sm disabled:opacity-50"
        >
          Reassign
        </button>
      </div>
      <span id="reassign-why" class="text-term-fg-muted text-xs">{reassignOff ?? ''}</span>
    </div>

    <div class="flex flex-col gap-1">
      {#if confirmCancel}
        <div class="flex gap-2">
          <button
            type="button"
            onclick={() => {
              confirmCancel = false;
              run('cancel');
            }}
            disabled={!cancel.enabled}
            class="border-term-red/50 text-term-red min-h-11 border px-4 py-2 text-sm disabled:opacity-50"
          >
            Confirm cancel
          </button>
          <button
            type="button"
            onclick={() => (confirmCancel = false)}
            class="border-term-border text-term-fg min-h-11 border px-4 py-2 text-sm"
          >
            Keep running
          </button>
        </div>
      {:else}
        <button
          type="button"
          onclick={() => (confirmCancel = true)}
          disabled={!cancel.enabled}
          aria-describedby="cancel-why"
          class="border-term-border text-term-fg hover:border-term-red hover:text-term-red min-h-11 border px-4 py-2 text-sm disabled:opacity-50"
        >
          Cancel task
        </button>
      {/if}
      <span id="cancel-why" class="text-term-fg-muted text-xs">{cancel.reason ?? ''}</span>
    </div>
  </div>

  {#if busy}
    <p class="text-term-fg-muted mt-3 text-sm" role="status">Sending request…</p>
  {/if}

  {#if actionState?.notice}
    <div
      class="mt-3 flex items-start justify-between gap-2 border px-3 py-2 text-sm {actionState.notice
        .tone === 'error'
        ? 'border-term-red/50 text-term-red'
        : actionState.notice.tone === 'warning'
          ? 'border-term-yellow/50 text-term-yellow'
          : 'border-term-border text-term-fg'}"
      role={actionState.notice.tone === 'info' ? 'status' : 'alert'}
      data-testid="task-action-notice"
    >
      <span>{actionState.notice.text}</span>
      <button
        type="button"
        class="shrink-0 underline"
        onclick={() =>
          actionState?.resend
            ? tasks.dismissPending(taskId, actionState.resend)
            : tasks.clearNotice(taskId)}
      >
        {actionState?.resend ? 'Discard' : 'Dismiss'}
      </button>
    </div>
  {/if}

  {#if blockers.length > 0}
    <div class="mt-3" data-testid="task-blockers">
      <p class="text-term-fg-muted text-xs">Blocking a provider switch:</p>
      <ul class="text-term-yellow mt-1 list-disc pl-5 text-sm">
        {#each blockers as blocker (blocker)}<li>{blocker}</li>{/each}
      </ul>
    </div>
  {/if}
</section>
