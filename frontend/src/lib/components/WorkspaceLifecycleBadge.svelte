<script lang="ts">
  import type { WorkspaceLifecycle } from '$lib/api';

  let { workspace }: { workspace: WorkspaceLifecycle } = $props();

  const label = $derived(
    workspace.observed_state === 'suspended'
      ? workspace.snapshot_ref
        ? 'PARKED'
        : 'SUSPENDED · SNAPSHOT UNKNOWN'
      : workspace.observed_state.toUpperCase()
  );

  const color = $derived(
    workspace.observed_state === 'running'
      ? 'border-term-cyan/50 text-term-cyan'
      : workspace.observed_state === 'suspended'
        ? 'border-term-purple/50 text-term-purple'
        : workspace.observed_state === 'failed'
          ? 'border-term-red/50 text-term-red'
          : 'border-term-yellow/50 text-term-yellow'
  );

  const detail = $derived(
    workspace.conditions.find((condition) => condition.type === 'ControlOperation')?.message ??
      workspace.conditions.find((condition) => condition.type === 'Available')?.message ??
      `Desired ${workspace.desired_state}; observed ${workspace.observed_state}.`
  );
</script>

<span
  class="inline-flex max-w-full items-center border px-2 py-0.5 text-xs font-medium {color}"
  title={detail}
  aria-label={`Workspace ${label.toLowerCase()}`}
  data-testid="workspace-lifecycle-badge"
>
  <span class="truncate">WS {label}</span>
</span>
