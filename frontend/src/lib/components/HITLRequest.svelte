<script lang="ts">
  import {
    buildHITLResponse,
    hitlStatus,
    mergeContexts,
    parseHITL,
    reasonNotice,
    requestTools,
    type HITLDraft,
    type HITLState
  } from '../hitl';
  let {
    snapshot,
    onRespond,
    onRefresh
  }: { snapshot: HITLState; onRespond: (draft: HITLDraft) => void; onRefresh: () => void } =
    $props();
  let decisions = $state<HITLDraft['decisions']>(Object.create(null));
  let reasons = $state<Record<string, string>>(Object.create(null));
  let answers = $state<string[][]>([]);
  const view = $derived(snapshot.view);
  const payload = $derived(parseHITL(view?.request.payload));
  const disabled = $derived(
    snapshot.busy ||
      snapshot.stale ||
      snapshot.uncertain ||
      !view?.answerable ||
      !view?.writes_enabled ||
      !!view?.response
  );
  const validation = $derived.by(() => {
    try {
      buildHITLResponse(view?.request.payload, { decisions, reasons, answers });
      return null;
    } catch (error) {
      return (error as Error).message;
    }
  });
  function choose(index: number, choice: string, multiple: boolean) {
    const current = answers[index] ?? [];
    answers[index] = multiple
      ? current.includes(choice)
        ? current.filter((a) => a !== choice)
        : [...current, choice]
      : [choice];
  }
</script>

<section class="hitl" aria-label="Session input">
  {#if view}
    <header>
      <h3>{payload?.type === 'ask_user_request' ? 'Questions for you' : 'Session needs input'}</h3>
      <p class="status" role="status">{hitlStatus(view)}</p>
    </header>
    <p class="context">
      {#if view.context?.project_id}<a
          href="/projects/{encodeURIComponent(view.context.project_id)}"
          >{view.context.project_name ?? 'Project'}</a
        > ·
      {/if}
      {view.context?.title ?? 'Observed session'}
    </p>
    <details>
      <summary>Session and task</summary>
      <p>Session: <code>{view.request.outer.runtime_session_id}</code></p>
      <p>Task: <code>{view.request.outer.task_id}</code></p>
    </details>
    {#if view.context?.session_id}
      <a
        href={view.context.role === 'main'
          ? '/'
          : `/sessions/${encodeURIComponent(view.context.session_id)}`}
        >Open original conversation</a
      >
    {:else}
      <p class="context">This observed session has no Mainloop conversation.</p>
    {/if}
    {#if view.route_request_id !== view.request.id}
      <a href="/?hitl={encodeURIComponent(view.route_request_id)}">Open continuation request</a>
    {/if}
    {#if view.unavailable_reason}<p class="context">{view.unavailable_reason}</p>{/if}
    {#if !view.writes_enabled && !view.response}<p>Responding disabled by the server.</p>{/if}
    {#each mergeContexts(view) as context}
      <section
        aria-label={context.facts ? 'Verified merge context' : 'Merge context unavailable'}
        class="merge"
      >
        <h4>Merge context · call {context.toolId}</h4>
        {#if context.facts}
          {@const facts = context.facts}
          <a
            href={`https://github.com/${facts.repository}/pull/${facts.pr_number}`}
            target="_blank"
            rel="noopener noreferrer">{facts.repository} #{facts.pr_number}</a
          >
          {#if facts.stale || snapshot.stale || view.request.availability !== 'pending'}
            <p role="status">
              Current merge context unavailable. These are previously recorded facts.
            </p>
          {/if}
          <p>Head: {facts.head} · <code>{facts.head_sha}</code></p>
          <p>Base: {facts.base} · <code>{facts.base_sha}</code></p>
          <p>Protected paths: {facts.protected_matches.join(', ') || 'None recorded'}</p>
          {#if facts.ci && typeof facts.ci.green === 'boolean'}
            <p>
              Recorded CI evidence: {facts.ci.green
                ? 'Checks passed at preparation'
                : 'Checks not passing at preparation'}.
            </p>
            <details>
              <summary>Recorded checks and statuses</summary>
              <pre>{JSON.stringify(facts.ci, null, 2)}</pre>
            </details>
          {:else}
            <p>CI evidence unavailable.</p>
          {/if}
          <p class="context">
            Recorded evidence is not merge approval. Current facts are checked when you respond and
            when the merge runs.
          </p>
        {:else}
          <p>Verified merge context unavailable.</p>
        {/if}
      </section>
    {/each}
    {#if !payload}
      <p role="alert">Malformed or unsupported request. Controls are unavailable.</p>
    {:else if payload.type === 'tool_approval_request'}
      {#if payload.hint}<p class="context">Agent note: {payload.hint}</p>{/if}
      {#each requestTools(payload) as tool (tool.id)}
        <fieldset {disabled}>
          <legend>{tool.name}</legend>
          <p class="context">Call: <code>{tool.call_id}</code></p>
          <pre>{JSON.stringify(tool.args, null, 2)}</pre>
          <div class="choices">
            <button
              type="button"
              aria-pressed={decisions[tool.id] === 'approve'}
              onclick={() => {
                decisions = { ...decisions, [tool.id]: 'approve' };
                const { [tool.id]: removed, ...remaining } = reasons;
                reasons = remaining;
              }}>Approve</button
            >
            <button
              type="button"
              aria-pressed={decisions[tool.id] === 'reject'}
              onclick={() => (decisions = { ...decisions, [tool.id]: 'reject' })}>Reject</button
            >
          </div>
          {#if decisions[tool.id] === 'reject'}
            <label
              >Rejection reason (optional)<textarea
                maxlength="8192"
                rows="2"
                value={Object.hasOwn(reasons, tool.id) ? reasons[tool.id] : ''}
                oninput={(event) =>
                  (reasons = { ...reasons, [tool.id]: event.currentTarget.value })}
              ></textarea></label
            >
          {/if}
        </fieldset>
      {/each}
      <p class="context">{reasonNotice(view.context?.provider)}</p>
    {:else}
      {#each payload.questions as question, i}
        <fieldset {disabled}>
          <legend>{question.question}</legend>
          {#if question.choices?.length}
            <p class="context">{question.multiple ? 'Choose one or more.' : 'Choose one.'}</p>
            <div class="choices">
              {#each question.choices as choice}
                <button
                  type="button"
                  aria-pressed={answers[i]?.includes(choice) ?? false}
                  onclick={() => choose(i, choice, question.multiple)}>{choice}</button
                >
              {/each}
            </div>
          {:else}
            <label
              >Your answer<textarea
                maxlength="8192"
                rows="3"
                value={answers[i]?.[0] ?? ''}
                oninput={(event) => (answers[i] = [event.currentTarget.value])}
              ></textarea></label
            >
          {/if}
        </fieldset>
      {/each}
    {/if}
    {#if view.response}
      <section aria-label="Recorded response">
        <h4>Your recorded response</h4>
        {#if view.response.response.type === 'tool_approval_response'}
          {#each view.response.response.approvals as approval}
            <p><code>{approval.id}</code>: {approval.approved ? 'Approved' : 'Rejected'}</p>
            {#if approval.rejection_reason}<p class="reason">{approval.rejection_reason}</p>{/if}
          {/each}
        {:else}
          {#each view.response.response.answers as answer, i}<p>
              Answer {i + 1}: {answer.answer.join(', ')}
            </p>{/each}
        {/if}
        <p class="context">
          Delivery status applies to this decision, not to completion of the task.
        </p>
      </section>
    {:else if payload}
      {#if validation && !disabled}<p class="context">{validation}</p>{/if}
      <button
        type="button"
        class="submit"
        disabled={disabled || !!validation}
        onclick={() => onRespond({ decisions, reasons, answers })}
        >{snapshot.busy ? 'Recording…' : 'Send response'}</button
      >
    {/if}
    <details>
      <summary>Request data (agent supplied)</summary>
      <pre>{JSON.stringify(view.request.payload, null, 2)}</pre>
    </details>
  {:else}<p role="status">Loading session input…</p>{/if}
  {#if snapshot.error}<p role="alert">{snapshot.error}</p>{/if}
  <button type="button" disabled={snapshot.busy} onclick={onRefresh}>Refresh status</button>
</section>

<style>
  .hitl {
    color: var(--color-term-fg);
    padding: 1rem;
    font-size: 0.875rem;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  header,
  fieldset,
  .merge {
    margin-bottom: 1rem;
  }
  h3,
  h4,
  legend {
    font-weight: 600;
  }
  h3 {
    font-size: 1rem;
  }
  p,
  details,
  a,
  label {
    margin-block: 0.5rem;
  }
  .context {
    color: var(--color-term-fg-muted);
  }
  .status,
  a {
    color: var(--color-term-accent);
  }
  a {
    display: inline-block;
    text-decoration: underline;
    text-underline-offset: 0.2em;
  }
  fieldset {
    min-width: 0;
    border-top: 1px solid var(--color-term-border);
    padding-top: 0.75rem;
    margin-top: 1rem;
  }
  legend {
    padding-right: 0.5rem;
  }
  pre {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    max-height: 16rem;
    overflow: auto;
    background: var(--color-term-bg-secondary);
    padding: 0.75rem;
  }
  button,
  textarea {
    border: 1px solid var(--color-term-border);
    background: var(--color-term-bg);
    color: var(--color-term-fg);
    padding: 0.65rem 0.75rem;
  }
  button {
    min-height: 44px;
    cursor: pointer;
  }
  button:hover:not(:disabled),
  button[aria-pressed='true'] {
    border-color: var(--color-term-accent);
    color: var(--color-term-accent);
  }
  button[aria-pressed='true'] {
    background: var(--color-term-bg-secondary);
    box-shadow: inset 0 -2px var(--color-term-accent);
  }
  button:disabled,
  fieldset:disabled {
    opacity: 0.65;
    cursor: default;
  }
  button:focus-visible,
  textarea:focus-visible,
  summary:focus-visible,
  a:focus-visible {
    outline: 2px solid var(--color-term-accent);
    outline-offset: 3px;
  }
  label,
  textarea {
    display: block;
    width: 100%;
  }
  textarea {
    margin-top: 0.4rem;
    resize: vertical;
  }
  .choices {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
  }
  .submit {
    margin-block: 0.75rem;
  }
  .reason {
    white-space: pre-wrap;
  }
  summary {
    cursor: pointer;
    min-height: 32px;
  }
  [role='alert'] {
    color: var(--color-term-red);
  }
</style>
