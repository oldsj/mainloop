<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type NativeSessionInfo } from '$lib/api';

  let {
    sessionId,
    collapsible = true
  }: {
    sessionId: string;
    /** Show one summary line with a details toggle; false shows every field (the caller toggles). */
    collapsible?: boolean;
  } = $props();
  let info = $state<NativeSessionInfo | null>(null);
  let open = $state(false);

  const expanded = $derived(!collapsible || open);
  const live = $derived(
    info?.agent_live === null || info?.agent_live === undefined
      ? 'unknown'
      : info.agent_live
        ? 'live'
        : 'idle'
  );

  async function refresh() {
    try {
      info = await api.getSessionNative(sessionId);
    } catch (e) {
      console.error('Failed to load native session info:', e);
    }
  }

  onMount(() => {
    refresh();
    const timer = setInterval(refresh, 3000);
    return () => clearInterval(timer);
  });
</script>

{#if info}
  <div
    class="border-term-border bg-term-bg-secondary text-term-fg-muted border-b px-4 py-2 font-mono text-xs"
    data-testid="identity-strip"
  >
    {#if collapsible}
      <div class="flex items-center gap-2">
        <span
          class="h-2 w-2 shrink-0 rounded-full {live === 'live' ? 'bg-term-green' : 'bg-term-fg-muted'}"
          aria-hidden="true"
        ></span>
        <span class="text-term-fg">{info.kind}</span>
        <span>·</span>
        <span class="truncate">{(info.model ?? 'model pending').replace(/^claude-/, '')}</span>
        <span>·</span>
        <span>{live}</span>
        {#if info.topic}
          <span class="hidden sm:inline">·</span>
          <span class="hidden truncate sm:inline">#{info.topic}</span>
        {/if}
        <button
          type="button"
          class="hover:text-term-accent ml-auto shrink-0"
          aria-expanded={open}
          onclick={() => (open = !open)}
          data-testid="identity-toggle"
        >
          details {open ? '▴' : '▾'}
        </button>
      </div>
    {/if}
    <div class="flex flex-wrap gap-x-4 gap-y-1 {collapsible ? 'mt-2' : ''} {expanded ? '' : 'hidden'}">
      <span>agent <b class="text-term-accent" data-testid="id-kind">{info.kind}</b></span>
      <span
        >model <b class="text-term-fg" data-testid="id-model">{info.model ?? 'unknown yet'}</b
        ></span
      >
      <span>policy <b class="text-term-fg" data-testid="id-policy">{info.approval_policy}</b></span>
      <span
        >native session <b class="text-term-fg" data-testid="id-native"
          >{info.native_session_id ?? 'pending'}</b
        ></span
      >
      <span
        >herdr pane <b class="text-term-fg">{info.herdr_pane_id ?? '-'}</b>
        ({info.agent_name})</span
      >
      <span>
        pod <b class="text-term-fg" data-testid="id-pod">{info.workspace_pod}</b>
        {info.workspace_pod_uid ? info.workspace_pod_uid.slice(0, 8) : '-'}
        {info.workspace_ready ? 'ready' : 'not ready'}
      </span>
      <span
        >agent {info.agent_live === null
          ? 'unknown'
          : info.agent_live
            ? 'live'
            : 'not running (resumes on next message)'}</span
      >
      <span>gen <b class="text-term-fg" data-testid="id-gen">{info.generation}</b></span>
      {#if info.role && info.role !== 'agent'}
        <span>role <b class="text-term-accent" data-testid="id-role">{info.role}</b></span>
      {/if}
      {#if info.parent_session_id}
        <span
          >parent <b class="text-term-fg" data-testid="id-parent"
            >{info.parent_session_id.slice(0, 8)}</b
          ></span
        >
      {/if}
      {#if info.topic}
        <span>topic <b class="text-term-fg" data-testid="id-topic">{info.topic}</b></span>
      {/if}
      {#if info.role === 'main'}
        <span>
          window <b class="text-term-fg" data-testid="id-lineage">#{info.lineage_seq}</b>
          {info.turns_in_lineage} turns, context {info.context_tokens ?? '-'} (baseline {info.baseline_tokens ??
            '-'})
          {info.rotating ? 'ROTATING' : ''}
        </span>
        <span
          >native compactions <b class="text-term-fg" data-testid="id-compactions"
            >{info.continuations ?? 0}</b
          ></span
        >
      {/if}
      <span>journal {info.journal_ref ?? '-'} @ {info.journal_cursor}</span>
    </div>
    {#if info.note}
      <div class="text-term-yellow mt-1" data-testid="id-note">{info.note}</div>
    {/if}
    {#if info.deliveries.length}
      <div class="mt-1 {expanded ? '' : 'hidden'}" data-testid="id-deliveries">
        deliveries: {info.deliveries.length}
        {#each info.deliveries.slice(-3) as d (d.message_id)}
          <span class="mr-2" title={d.detail ?? d.evidence_ref ?? ''}>{d.state}</span>
        {/each}
      </div>
    {/if}
  </div>
{/if}
