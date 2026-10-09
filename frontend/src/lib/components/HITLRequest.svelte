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
  import type {
    HITLMergeDetailSection,
    HITLMergeDetails,
    VerifiedMergeContext
  } from '../hitl';
  let {
    snapshot,
    onRespond,
    onRefresh,
    onLoadMergeDetails
  }: {
    snapshot: HITLState;
    onRespond: (draft: HITLDraft) => void;
    onRefresh: () => void;
    onLoadMergeDetails: (
      proposalId: string,
      section: HITLMergeDetailSection,
      cursor: number
    ) => Promise<HITLMergeDetails>;
  } =
    $props();
  let decisions = $state<HITLDraft['decisions']>(Object.create(null));
  let reasons = $state<Record<string, string>>(Object.create(null));
  let answers = $state<string[][]>([]);
  type DetailSection = HITLMergeDetailSection;
  type DetailState = {
    loaded: boolean;
    loading: boolean;
    error: string | null;
    description: string | null;
    items: Record<string, unknown>[];
    nextCursor: string | null;
    truncated: boolean;
    capturedAt: string | null;
  };
  const emptyDetails = (): Record<DetailSection, DetailState> => ({
    description: {
      loaded: false,
      loading: false,
      error: null,
      description: null,
      items: [],
      nextCursor: null,
      truncated: false,
      capturedAt: null
    },
    files: {
      loaded: false,
      loading: false,
      error: null,
      description: null,
      items: [],
      nextCursor: null,
      truncated: false,
      capturedAt: null
    },
    checks: {
      loaded: false,
      loading: false,
      error: null,
      description: null,
      items: [],
      nextCursor: null,
      truncated: false,
      capturedAt: null
    }
  });
  let details = $state<Record<string, Record<DetailSection, DetailState>>>({});
  const view = $derived(snapshot.view);
  const payload = $derived(parseHITL(view?.request.payload));
  const mergeReviews = $derived(view ? mergeContexts(view) : []);
  const disabled = $derived(
    snapshot.busy ||
      snapshot.stale ||
      snapshot.uncertain ||
      !view?.answerable ||
      !view?.writes_enabled ||
      !!view?.response
  );
  const reviewProblem = $derived.by(() => {
    const selected = mergeReviews.some(
      (context) =>
        decisions[context.toolId] === 'approve' && !isReviewable(context.facts)
    );
    if (selected)
      return 'This merge summary is stale or unavailable. Reject the call or refresh the request.';
    return null;
  });
  const validation = $derived.by(() => {
    if (reviewProblem) return reviewProblem;
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
  function isReviewable(context: VerifiedMergeContext | null) {
    return !!(
      context &&
      context.availability === 'ready' &&
      !context.stale &&
      context.summary?.availability === 'ready' &&
      context.summary_digest
    );
  }
  function selectedReviewContext() {
    return Object.fromEntries(
      mergeReviews.flatMap(({ toolId, facts }) =>
        decisions[toolId] === 'approve' && isReviewable(facts)
          ? [[toolId, facts!.summary_digest!]]
          : []
      )
    );
  }
  function detailState(proposalId: string, section: DetailSection) {
    return (details[proposalId] ?? emptyDetails())[section];
  }
  function saveDetailState(proposalId: string, section: DetailSection, value: DetailState) {
    details = {
      ...details,
      [proposalId]: { ...(details[proposalId] ?? emptyDetails()), [section]: value }
    };
  }
  async function loadDetails(context: VerifiedMergeContext, section: DetailSection, cursor = 0) {
    const current = detailState(context.proposal_id, section);
    if (current.loading || (cursor === 0 && current.loaded)) return;
    saveDetailState(context.proposal_id, section, { ...current, loading: true, error: null });
    try {
      const response = await onLoadMergeDetails(context.proposal_id, section, cursor);
      const latest = detailState(context.proposal_id, section);
      saveDetailState(context.proposal_id, section, {
        ...latest,
        loaded: true,
        loading: false,
        error: null,
        description: response.description ?? latest.description,
        items:
          cursor > 0
            ? [...latest.items, ...(response.items ?? [])]
            : (response.items ?? []),
        nextCursor: response.next_cursor,
        truncated: response.truncated,
        capturedAt: response.captured_at
      });
    } catch (error) {
      const latest = detailState(context.proposal_id, section);
      saveDetailState(context.proposal_id, section, {
        ...latest,
        loading: false,
        error: (error as Error).message
      });
    }
  }
  function toggleDetails(
    event: Event & { currentTarget: EventTarget & HTMLDetailsElement },
    context: VerifiedMergeContext,
    section: DetailSection
  ) {
    if (event.currentTarget.open) void loadDetails(context, section);
  }
  function loadMore(context: VerifiedMergeContext, section: DetailSection) {
    const cursor = Number(detailState(context.proposal_id, section).nextCursor);
    if (Number.isSafeInteger(cursor) && cursor > 0) void loadDetails(context, section, cursor);
  }
  function safeExternalHref(value: unknown): string | null {
    if (typeof value !== 'string') return null;
    try {
      const parsed = new URL(value);
      return parsed.protocol === 'https:' ? parsed.toString() : null;
    } catch {
      return null;
    }
  }
  function conversationHref() {
    if (!view?.context?.session_id) return null;
    return view.context.role === 'main'
      ? '/'
      : `/sessions/${encodeURIComponent(view.context.session_id)}`;
  }
  function approvalReason(summary: NonNullable<VerifiedMergeContext['summary']>) {
    const reasons = summary.approval_reasons.map((reason) =>
      reason.type === 'project_policy'
        ? 'Project policy requires approval.'
        : `Protected path matched ${reason.glob}: ${reason.path}.`
    );
    return reasons.length ? reasons : ['An owner decision is required for this proposal.'];
  }
  function formatCapturedAt(value: string | null | undefined) {
    if (!value) return 'time unavailable';
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
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
    {#each mergeReviews as context (context.toolId)}
      <section
        aria-label={context.facts ? 'Verified merge context' : 'Merge context unavailable'}
        class="merge"
      >
        {#if context.facts}
          {@const facts = context.facts}
          {@const summary = facts.summary}
          {#if summary}
            {@const descriptionDetail = detailState(facts.proposal_id, 'description')}
            {@const filesDetail = detailState(facts.proposal_id, 'files')}
            {@const checksDetail = detailState(facts.proposal_id, 'checks')}
            <h4>{summary.title || 'PR title unavailable'}</h4>
            {#if summary.description_excerpt}<p class="context">
                PR description excerpt: {summary.description_excerpt}{summary.description_truncated
                  ? ' … (description truncated)'
                  : ''}
              </p>{/if}
            <p>
              <strong>{facts.repository} #{facts.pr_number}</strong> · {summary.file_count} files ·
              +{summary.additions} / −{summary.deletions}
              {#if summary.paths_preview.length}
                · including {summary.paths_preview.join(', ')}{summary.paths_preview_truncated
                  ? ', …'
                  : ''}
              {/if}
            </p>
            <p class="approval-reason">Approval required because:</p>
            {#each approvalReason(summary) as reason}<p class="approval-reason">{reason}</p>{/each}
            {#if facts.availability === 'stale' || facts.availability === 'unavailable'}
              <p class="context" role="status">
                {facts.freshness_reason ?? 'Approval context is unavailable. Refresh this request.'}
              </p>
            {/if}
            {#if facts.deadline}
              <p class="context">Evaluation deadline: {formatCapturedAt(facts.deadline)}</p>
            {/if}
            <p>Head: {summary.head} · <code>{summary.head_sha}</code></p>
            <p>Base: {summary.base} · <code>{summary.base_sha}</code></p>
            <p class="context">
              Proposal <code>{facts.proposal_id}</code> · project policy v{summary.policy_version} ·
              protected globs v{summary.globs_version}
            </p>
            <p class="context">Reviewed summary SHA-256: <code>{facts.summary_digest}</code></p>
            {#if summary.ci?.complete}
              <p>
                {summary.ci.result_count} CI results recorded at {formatCapturedAt(
                  summary.ci.captured_at
                )} · {summary.ci.passed_count} passed, {summary.ci.failed_count} failed,
                {summary.ci.pending_count} pending at preparation.
              </p>
            {:else}
              <p class="context">Recorded CI evidence is unavailable.</p>
            {/if}
            <nav class="merge-links" aria-label="Pull request links">
              {#if safeExternalHref(facts.pr_url)}<a
                  href={safeExternalHref(facts.pr_url)!}
                  target="_blank"
                  rel="noopener noreferrer">Open PR</a
                >{/if}
              {#if safeExternalHref(facts.compare_url)}<a
                  href={safeExternalHref(facts.compare_url)!}
                  target="_blank"
                  rel="noopener noreferrer">Compare diff</a
                >{/if}
              {#if conversationHref()}<a href={conversationHref()!}>Open agent conversation</a>{/if}
            </nav>
            <details ontoggle={(event) => toggleDetails(event, facts, 'description')}>
              <summary>PR description from GitHub</summary>
              {#if descriptionDetail.loading}<p role="status">Loading description…</p>
              {:else if descriptionDetail.error}<p role="alert">{descriptionDetail.error}</p>
              {:else if descriptionDetail.loaded}
                <pre class="description">{descriptionDetail.description}</pre>
                {#if descriptionDetail.truncated}<p class="context">The saved description is truncated.</p>{/if}
              {:else}<p class="context">Expand to load the captured PR description.</p>{/if}
            </details>
            <details ontoggle={(event) => toggleDetails(event, facts, 'files')}>
              <summary>Changed paths</summary>
              {#if filesDetail.loading}<p role="status">Loading paths…</p>
              {:else if filesDetail.error}<p role="alert">{filesDetail.error}</p>
              {:else if filesDetail.loaded}
                <ul class="detail-list">
                  {#each filesDetail.items as file}
                    <li>
                      <code>{String(file.filename ?? 'Unknown path')}</code>
                      <span>{String(file.status ?? 'changed')} · +{String(file.additions ?? 0)} / −{String(file.deletions ?? 0)}</span>
                      {#if typeof file.previous_filename === 'string'}<span
                          >Renamed from <code>{file.previous_filename}</code></span
                        >{/if}
                    </li>
                  {/each}
                </ul>
                {#if filesDetail.truncated}<p class="context">The captured path inventory is incomplete.</p>{/if}
                {#if filesDetail.nextCursor}<button
                    type="button"
                    disabled={filesDetail.loading}
                    onclick={() => loadMore(facts, 'files')}>Load more paths</button
                  >{/if}
              {:else}<p class="context">Expand to load captured paths.</p>{/if}
            </details>
            <details ontoggle={(event) => toggleDetails(event, facts, 'checks')}>
              <summary>Full CI list</summary>
              {#if checksDetail.loading}<p role="status">Loading CI results…</p>
              {:else if checksDetail.error}<p role="alert">{checksDetail.error}</p>
              {:else if checksDetail.loaded}
                <ul class="detail-list">
                  {#each checksDetail.items as item}
                    {@const href = safeExternalHref(item.html_url ?? item.target_url)}
                    <li>
                      <strong>{String(item.kind ?? 'CI result')}:</strong>
                      {String(item.name ?? item.context ?? `#${String(item.id ?? '')}`)}
                      · {String(item.conclusion ?? item.state ?? item.status ?? 'unknown')}
                      {#if item.app_id !== undefined}<span> · app {String(item.app_id)}</span>{/if}
                      {#if href}<a href={href} target="_blank" rel="noopener noreferrer">Open result</a>{/if}
                    </li>
                  {/each}
                </ul>
                {#if checksDetail.truncated}<p class="context">The captured CI inventory is incomplete.</p>{/if}
                {#if checksDetail.nextCursor}<button
                    type="button"
                    disabled={checksDetail.loading}
                    onclick={() => loadMore(facts, 'checks')}>Load more CI results</button
                  >{/if}
              {:else}<p class="context">Expand to load the complete captured CI list.</p>{/if}
            </details>
            <p class="context">
              This is the captured proposal context, not a claim that the merge is safe or complete.
              The server checks current evidence again when you respond and when the merge runs.
            </p>
          {:else}
            <p>Verified approval summary unavailable.</p>
          {/if}
        {:else}
          <h4>Merge context unavailable · call {context.toolId}</h4>
          <p>Verified merge context is unavailable. Approve is disabled; Reject remains available.</p>
        {/if}
      </section>
    {/each}
    {#if !payload}
      <p role="alert">Malformed or unsupported request. Controls are unavailable.</p>
    {:else if payload.type === 'tool_approval_request'}
      {#if payload.hint}<p class="context">Agent note: {payload.hint}</p>{/if}
      {#each requestTools(payload) as tool (tool.id)}
        {@const mergeContext = mergeReviews.find((item) => item.toolId === tool.id)}
        <fieldset {disabled}>
          <legend>{tool.name}</legend>
          <p class="context">Call: <code>{tool.call_id}</code></p>
          <pre>{JSON.stringify(tool.args, null, 2)}</pre>
          <div class="choices">
            <button
              type="button"
              disabled={!!mergeContext && !isReviewable(mergeContext.facts)}
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
        onclick={() =>
          onRespond({ decisions, reasons, answers, reviewedContext: selectedReviewContext() })}
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
  .approval-reason {
    font-weight: 600;
  }
  .merge-links {
    display: flex;
    flex-wrap: wrap;
    gap: 0.25rem 1rem;
  }
  .description {
    max-height: 20rem;
  }
  .detail-list {
    padding-left: 1.25rem;
  }
  .detail-list li {
    display: grid;
    gap: 0.2rem;
    margin-block: 0.6rem;
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
