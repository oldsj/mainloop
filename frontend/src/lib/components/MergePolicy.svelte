<script lang="ts">
  import { api } from '../api';
  import type { MergePolicyView } from '../hitl';
  let { projectId }: { projectId: string } = $props();
  let policy = $state<MergePolicyView | null>(null);
  let choice = $state<'auto' | 'approval'>('auto');
  let busy = $state(false);
  let error = $state<string | null>(null);
  let stale = $state(true);
  async function load(id: string) {
    try {
      const fresh = await api.getMergePolicy(id);
      if (id !== projectId) return;
      policy = fresh;
      choice = fresh.merge_policy;
      stale = false;
    } catch {
      if (id === projectId) {
        stale = true;
        error = 'Could not load the current policy. Refresh before editing.';
      }
    }
  }
  $effect(() => {
    const id = projectId;
    policy = null;
    stale = true;
    error = null;
    void load(id);
  });
  async function save() {
    if (!policy?.writes_enabled || busy || stale) return;
    const id = projectId;
    busy = true;
    error = null;
    try {
      const fresh = await api.updateMergePolicy(id, choice, policy.merge_policy_version);
      if (id === projectId) {
        policy = fresh;
        choice = fresh.merge_policy;
      }
    } catch (cause) {
      if (id === projectId) {
        error = `${(cause as Error).message}. Review the current policy before saving again.`;
        stale = true;
        await load(id);
      }
    } finally {
      busy = false;
    }
  }
</script>

<section class="border-term-border mb-6 border-b pb-6" aria-label="Merge policy">
  <h2 class="text-term-fg mb-3 text-sm font-semibold">Merge policy</h2>
  {#if policy}
    <p class="text-term-fg mb-2 text-sm">
      Current policy: {policy.merge_policy === 'auto' ? 'Auto' : 'Approval required'}
    </p>
    <p class="text-term-fg-muted mb-3 text-sm">
      Auto permits eligible merges after server checks. Approval requires an owner decision.
      Protected paths always require approval.
    </p>
    <label class="text-term-fg block text-sm"
      >Policy
      <select
        class="border-term-border bg-term-bg mt-2 block min-h-11 border px-3 py-2 disabled:opacity-60"
        bind:value={choice}
        disabled={!policy.writes_enabled || busy || stale}
      >
        <option value="auto">Auto</option><option value="approval">Approval required</option>
      </select>
    </label>
    {#if !policy.writes_enabled}<p class="text-term-fg-muted mt-2 text-sm">
        Editing disabled by the server. The current policy remains in effect.
      </p>{/if}
    <button
      type="button"
      class="border-term-border text-term-fg mt-3 min-h-11 border px-3 py-2 text-sm disabled:opacity-50"
      disabled={!policy.writes_enabled || busy || stale || choice === policy.merge_policy}
      onclick={save}>{busy ? 'Saving…' : 'Save policy'}</button
    >
    <details class="text-term-fg-muted mt-3 text-sm">
      <summary>Protected paths (read-only)</summary>
      <ul>
        {#each policy.protected_globs as glob}<li><code>{glob}</code></li>{/each}
      </ul>
    </details>
  {:else}<p class="text-term-fg-muted text-sm">Loading merge policy…</p>{/if}
  {#if error}<p class="text-term-red mt-2 text-sm" role="alert">{error}</p>
    <button
      type="button"
      class="text-term-accent min-h-11 text-sm underline"
      disabled={busy}
      onclick={() => load(projectId)}>Refresh policy</button
    >{/if}
</section>
