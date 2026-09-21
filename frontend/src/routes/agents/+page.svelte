<script lang="ts">
  import { goto } from '$app/navigation';
  import { api } from '$lib/api';
  import { connection } from '$lib/stores/connection';

  let kind = $state<'claude' | 'codex'>('claude');
  let title = $state('');
  let prompt = $state('');
  let submitting = $state(false);
  let error = $state<string | null>(null);
  const offline = $derived($connection.status === 'offline');

  async function start() {
    if (!prompt.trim() || submitting || offline) return;
    submitting = true;
    error = null;
    try {
      const session = await api.createSession({
        title: title.trim() || `${kind} session`,
        description: `Native ${kind} agent under Herdr in the workspace pod`,
        prompt: prompt.trim(),
        agent_kind: kind
      });
      await goto(`/sessions/${session.id}`);
    } catch (e) {
      console.error('Failed to start agent session:', e);
      // Show the backend's reason (a policy refusal, say) rather than a generic failure.
      error = e instanceof Error && e.message ? e.message : 'Failed to start the agent session';
    } finally {
      submitting = false;
    }
  }
</script>

<svelte:head>
  <title>New agent session - mainloop</title>
</svelte:head>

<div class="mx-auto flex h-full max-w-2xl flex-col gap-4 bg-term-bg p-6 text-term-fg">
  <a href="/" class="text-sm text-term-fg-muted hover:text-term-accent">&larr; Back</a>
  <h1 class="text-lg font-medium">New agent session</h1>
  <p class="text-sm text-term-fg-muted">
    Starts a real agent in the workspace pod, under Herdr, in bypass-permissions mode. Replies are read from the
    agent's native journal.
  </p>

  <fieldset class="flex gap-4" disabled={submitting}>
    <legend class="mb-1 text-sm text-term-fg-muted">Agent</legend>
    <label class="flex items-center gap-2">
      <input type="radio" name="kind" value="claude" bind:group={kind} data-testid="kind-claude" /> Claude Code
    </label>
    <label class="flex items-center gap-2">
      <input type="radio" name="kind" value="codex" bind:group={kind} data-testid="kind-codex" /> Codex
    </label>
  </fieldset>

  <label class="flex flex-col gap-1 text-sm">
    Title (optional)
    <input
      class="border border-term-border bg-term-bg-secondary p-2 text-term-fg"
      bind:value={title}
      disabled={submitting}
      placeholder="Defaults to &quot;{kind} session&quot;"
      data-testid="agent-title"
    />
  </label>

  <label class="flex flex-col gap-1 text-sm">
    First message
    <textarea
      class="min-h-24 border border-term-border bg-term-bg-secondary p-2 text-term-fg"
      bind:value={prompt}
      disabled={submitting}
      onkeydown={(e) => {
        if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && !e.isComposing) {
          e.preventDefault();
          void start();
        }
      }}
      data-testid="agent-prompt"
    ></textarea>
  </label>

  {#if error}<p class="text-sm text-term-red">{error}</p>{/if}

  <button
    type="button"
    onclick={start}
    disabled={submitting || offline || !prompt.trim()}
    class="self-start border border-term-accent px-4 py-2 text-term-accent hover:bg-term-accent/10 disabled:opacity-50"
    data-testid="agent-start"
  >
    {submitting ? 'Starting…' : 'Start session'}
  </button>
  {#if offline}
    <p class="text-sm text-term-red">Backend unreachable: can't start a session right now.</p>
  {:else}
    <p class="text-xs text-term-fg-muted">Ctrl+Enter starts the session.</p>
  {/if}
</div>
