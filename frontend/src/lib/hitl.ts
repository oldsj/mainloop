/** Display and response contracts. Only the server decides whether a request is answerable. */
export interface HITLTool {
  id: string;
  call_id: string;
  name: string;
  args: Record<string, unknown>;
}
export interface HITLQuestion {
  question: string;
  choices: string[] | null;
  multiple: boolean;
}
export type HITLPayload = (
  | { type: 'tool_approval_request'; tools: HITLTool[]; hint?: string }
  | { type: 'ask_user_request'; id: string; questions: HITLQuestion[] }
) & {
  nested?: { tools: HITLTool[]; task_id: string; context_id: string } | null;
  [key: string]: unknown;
};
export type HITLResponse =
  | {
      type: 'tool_approval_response';
      approvals: { id: string; approved: boolean; rejection_reason?: string }[];
      reviewed_context?: Record<string, string>;
    }
  | { type: 'ask_user_response'; id: string; answers: { answer: string[] }[] };
export interface HITLView {
  request: {
    id: string;
    payload: unknown;
    availability: string;
    outer: {
      runtime_session_id: string;
      task_id: string;
      context_id: string;
      request_hash: string;
    };
    [key: string]: unknown;
  };
  response: { response: HITLResponse; [key: string]: unknown } | null;
  route_request_id: string;
  answerable: boolean;
  writes_enabled: boolean;
  transport_state: string | null;
  unavailable_reason: string | null;
  context?: {
    session_id?: string;
    title?: string;
    project_id?: string;
    project_name?: string;
    provider?: string | null;
    role?: string;
  };
  merge?: VerifiedMergeContext[] | null;
}
export interface MergeSummary {
  version: number;
  proposal_id: string;
  repository: string;
  pr_number: number;
  title: string;
  description_excerpt: string;
  description_truncated: boolean;
  description_digest: string;
  description_length: number;
  file_count: number;
  additions: number;
  deletions: number;
  paths_digest: string;
  paths_preview: string[];
  paths_preview_truncated: boolean;
  protected_matches: string[];
  approval_reasons: (
    | { type: 'project_policy' }
    | { type: 'protected_path'; glob: string; path: string }
  )[];
  policy: 'auto' | 'approval';
  policy_version: number;
  globs_version: number;
  head: string;
  head_sha: string;
  base: string;
  base_sha: string;
  ci: {
    complete: boolean;
    captured_at: string;
    result_count: number;
    passed_count: number;
    failed_count: number;
    pending_count: number;
    green_at_preparation: boolean;
    inventory_digest: string;
  } | null;
  availability: 'ready' | 'unavailable';
  unavailable_reasons: string[];
}
export interface VerifiedMergeContext {
  tool_id: string;
  proposal_id: string;
  summary: MergeSummary | null;
  summary_digest: string | null;
  availability: 'ready' | 'unavailable' | 'stale';
  freshness_reason: string | null;
  details_url: string | null;
  pr_url?: string;
  compare_url?: string;
  repository: string;
  pr_number: number;
  head: string;
  head_sha: string;
  base: string;
  base_sha: string;
  protected_matches: string[];
  stale: boolean;
}
export type HITLMergeDetailSection = 'description' | 'files' | 'checks';
export interface HITLMergeDetails {
  section: HITLMergeDetailSection;
  summary_digest: string;
  captured_at: string | null;
  description?: string;
  description_truncated?: boolean;
  truncated: boolean;
  items?: Record<string, unknown>[];
  next_cursor: string | null;
}

/** Missing, malformed or ambiguous resolver evidence must not look verified. */
export function mergeContexts(view: HITLView) {
  const payload = parseHITL(view.request.payload);
  const entries: unknown[] = Array.isArray(view.merge) ? view.merge : [];
  return (payload ? requestTools(payload) : []).flatMap((tool) => {
    const matches = entries.filter((v) => record(v) && v.tool_id === tool.id);
    // A public name is only a hint to show an unavailable notice, never evidence.
    if (!matches.length && !tool.name.endsWith('merge_pull_request_with_approval')) return [];
    const v = matches.length === 1 ? matches[0] : null;
    const valid =
      record(v) &&
      bounded(v.proposal_id) &&
      typeof v.repository === 'string' &&
      /^[\w.-]+\/[\w.-]+$/.test(v.repository) &&
      Number.isSafeInteger(v.pr_number) &&
      (v.pr_number as number) > 0 &&
      typeof v.summary_digest === 'string' &&
      /^[a-f0-9]{64}$/.test(v.summary_digest) &&
      validSummary(v.summary) &&
      (v.summary as MergeSummary).proposal_id === v.proposal_id &&
      ['ready', 'unavailable', 'stale'].includes(String(v.availability)) &&
      (v.freshness_reason === null || typeof v.freshness_reason === 'string') &&
      bounded(v.head) &&
      bounded(v.base) &&
      typeof v.head_sha === 'string' &&
      /^[a-f0-9]{40}$/.test(v.head_sha) &&
      typeof v.base_sha === 'string' &&
      /^[a-f0-9]{40}$/.test(v.base_sha) &&
      Array.isArray(v.protected_matches) &&
      v.protected_matches.every((p) => typeof p === 'string') &&
      typeof v.stale === 'boolean';
    return [{ toolId: tool.id, facts: valid ? (v as unknown as VerifiedMergeContext) : null }];
  });
}

function validSummary(value: unknown): value is MergeSummary {
  return (
    record(value) &&
    value.version === 1 &&
    bounded(value.proposal_id) &&
    typeof value.title === 'string' &&
    typeof value.description_excerpt === 'string' &&
    typeof value.description_truncated === 'boolean' &&
    typeof value.description_digest === 'string' &&
    /^[a-f0-9]{64}$/.test(value.description_digest) &&
    Number.isSafeInteger(value.description_length) &&
    Number.isSafeInteger(value.file_count) &&
    Number.isSafeInteger(value.additions) &&
    Number.isSafeInteger(value.deletions) &&
    typeof value.paths_digest === 'string' &&
    /^[a-f0-9]{64}$/.test(value.paths_digest) &&
    Array.isArray(value.paths_preview) &&
    value.paths_preview.every((path) => typeof path === 'string') &&
    typeof value.paths_preview_truncated === 'boolean' &&
    Array.isArray(value.protected_matches) &&
    value.protected_matches.every((path) => typeof path === 'string') &&
    Array.isArray(value.approval_reasons) &&
    value.approval_reasons.every(
      (reason) =>
        record(reason) &&
        (reason.type === 'project_policy' ||
          (reason.type === 'protected_path' &&
            typeof reason.glob === 'string' &&
            typeof reason.path === 'string'))
    ) &&
    (value.policy === 'auto' || value.policy === 'approval') &&
    Number.isSafeInteger(value.policy_version) &&
    Number.isSafeInteger(value.globs_version) &&
    bounded(value.head) &&
    typeof value.head_sha === 'string' &&
    /^[a-f0-9]{40}$/.test(value.head_sha) &&
    bounded(value.base) &&
    typeof value.base_sha === 'string' &&
    /^[a-f0-9]{40}$/.test(value.base_sha) &&
    (value.ci === null ||
      (record(value.ci) &&
        typeof value.ci.complete === 'boolean' &&
        typeof value.ci.captured_at === 'string' &&
        Number.isSafeInteger(value.ci.result_count) &&
        Number.isSafeInteger(value.ci.passed_count) &&
        Number.isSafeInteger(value.ci.failed_count) &&
        Number.isSafeInteger(value.ci.pending_count) &&
        typeof value.ci.green_at_preparation === 'boolean' &&
        typeof value.ci.inventory_digest === 'string' &&
        /^[a-f0-9]{64}$/.test(value.ci.inventory_digest))) &&
    (value.availability === 'ready' || value.availability === 'unavailable') &&
    Array.isArray(value.unavailable_reasons) &&
    value.unavailable_reasons.every((reason) => typeof reason === 'string')
  );
}

export interface MergePolicyView {
  merge_policy: 'auto' | 'approval';
  merge_policy_version: number;
  writes_enabled: boolean;
  protected_globs: string[];
  protected_globs_version: number;
}
export interface HITLDraft {
  decisions: Record<string, 'approve' | 'reject'>;
  reasons: Record<string, string>;
  answers: string[][];
  reviewedContext?: Record<string, string>;
}
const record = (v: unknown): v is Record<string, unknown> =>
  !!v && typeof v === 'object' && !Array.isArray(v);
const bounded = (v: unknown, max = 2048): v is string =>
  typeof v === 'string' && v.length > 0 && v.length <= max;
const toolsValid = (v: unknown): v is HITLTool[] =>
  Array.isArray(v) &&
  v.length > 0 &&
  v.length <= 100 &&
  v.every(
    (t) => record(t) && bounded(t.id) && bounded(t.call_id) && bounded(t.name) && record(t.args)
  ) &&
  new Set(v.map((t) => t.id)).size === v.length;
export function parseHITL(payload: unknown): HITLPayload | null {
  if (!record(payload)) return null;
  try {
    if (new TextEncoder().encode(JSON.stringify(payload)).length > 131072) return null;
  } catch {
    return null;
  }
  if (
    payload.nested != null &&
    (!record(payload.nested) ||
      !toolsValid(payload.nested.tools) ||
      !bounded(payload.nested.task_id) ||
      !bounded(payload.nested.context_id))
  )
    return null;
  if (
    payload.type === 'tool_approval_request' &&
    toolsValid(payload.tools) &&
    (payload.hint === undefined ||
      (typeof payload.hint === 'string' && payload.hint.length <= 4096))
  )
    return payload as HITLPayload;
  if (
    payload.type === 'ask_user_request' &&
    bounded(payload.id) &&
    Array.isArray(payload.questions) &&
    payload.questions.length > 0 &&
    payload.questions.length <= 100 &&
    payload.questions.every(
      (q) =>
        record(q) &&
        bounded(q.question, 8192) &&
        typeof q.multiple === 'boolean' &&
        (q.choices === null ||
          (Array.isArray(q.choices) &&
            q.choices.length <= 100 &&
            q.choices.every((c) => typeof c === 'string' && c.length <= 4096)))
    )
  ) {
    if (payload.nested && (payload.nested as { tools: HITLTool[] }).tools.length !== 1) return null;
    return payload as HITLPayload;
  }
  return null;
}
export function requestTools(p: HITLPayload): HITLTool[] {
  return p.type === 'tool_approval_request' ? (p.nested?.tools ?? p.tools) : [];
}
export function buildHITLResponse(payload: unknown, draft: HITLDraft): HITLResponse {
  const p = parseHITL(payload);
  if (!p) throw new Error('This request is malformed or unsupported.');
  if (p.type === 'tool_approval_request') {
    const tools = requestTools(p);
    if (Object.keys(draft.decisions).length !== tools.length)
      throw new Error('Choose approve or reject for every call.');
    return {
      type: 'tool_approval_response',
      approvals: tools.map((t) => {
        const decision = Object.hasOwn(draft.decisions, t.id) ? draft.decisions[t.id] : undefined;
        if (decision !== 'approve' && decision !== 'reject')
          throw new Error('Choose approve or reject for every call.');
        const reason = Object.hasOwn(draft.reasons, t.id) ? draft.reasons[t.id] : '';
        if (typeof reason !== 'string') throw new Error('Rejection reasons must be text.');
        if (reason.length > 8192)
          throw new Error('Rejection reasons must be at most 8192 characters.');
        return {
          id: t.id,
          approved: decision === 'approve',
          ...(decision === 'reject' && reason ? { rejection_reason: reason } : {})
        };
      }),
      ...(Object.keys(draft.reviewedContext ?? {}).length
        ? { reviewed_context: draft.reviewedContext }
        : {})
    };
  }
  if (draft.answers.length !== p.questions.length) throw new Error('Answer every question.');
  const answers = p.questions.map((q, i) => {
    const answer = draft.answers[i];
    if (
      !Array.isArray(answer) ||
      !answer.length ||
      answer.length > 100 ||
      (!q.multiple && answer.length !== 1) ||
      answer.some((a) => typeof a !== 'string' || !a.trim() || a.length > 8192) ||
      new Set(answer).size !== answer.length
    )
      throw new Error('Answer every question; select one answer unless multiple are allowed.');
    if (q.choices?.length && answer.some((a) => !q.choices!.includes(a)))
      throw new Error('Choose one of the offered answers.');
    return { answer: [...answer] };
  });
  return { type: 'ask_user_response', id: p.nested?.tools[0].id ?? p.id, answers };
}
export function reasonNotice(provider?: string | null): string {
  return provider === 'codex'
    ? 'Reason saved; runtime receives rejection only.'
    : provider === 'claude'
      ? 'The rejection reason is included in Claude’s denial message.'
      : 'Reason saved; delivery to this runtime is not verified.';
}
export function hitlStatus(v: HITLView): string {
  if (v.response)
    return (
      (
        {
          accepted: 'Decision delivered',
          recorded: 'Decision recorded; awaiting delivery',
          sending: 'Decision delivery uncertain',
          uncertain: 'Decision delivery uncertain',
          rejected_transport: 'Decision destination unavailable'
        } as Record<string, string>
      )[v.transport_state ?? ''] ?? 'Decision recorded'
    );
  if (v.route_request_id !== v.request.id) return 'Continue on the linked request';
  if (v.request.availability === 'stale') return 'Observation stale; waiting for a fresh check';
  if (!v.answerable) return 'Request unavailable';
  return v.writes_enabled ? 'Waiting for your response' : 'Responding disabled';
}
export interface HITLState {
  view: HITLView | null;
  busy: boolean;
  error: string | null;
  uncertain: boolean;
  stale: boolean;
}
/** One channel per request in this browser; backend receipts arbitrate other devices. */
export function createHITLChannel(
  read: (id: string, signal?: AbortSignal) => Promise<HITLView>,
  send: (id: string, action: string, response: HITLResponse) => Promise<HITLView>,
  // getRandomValues also works on the local HTTP mobile preview (randomUUID requires HTTPS).
  newId: () => string = () =>
    Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) =>
      byte.toString(16).padStart(2, '0')
    ).join('')
) {
  const entries = new Map<
    string,
    { state: HITLState; listeners: Set<(s: HITLState) => void>; version: number; reading: boolean }
  >();
  function entry(id: string) {
    let e = entries.get(id);
    if (!e) {
      e = {
        state: { view: null, busy: false, error: null, uncertain: false, stale: true },
        listeners: new Set(),
        version: 0,
        reading: false
      };
      entries.set(id, e);
    }
    return e;
  }
  function update(id: string, patch: Partial<HITLState>) {
    const e = entry(id);
    e.state = { ...e.state, ...patch };
    for (const fn of e.listeners) fn(e.state);
  }
  async function refresh(id: string, signal?: AbortSignal) {
    const e = entry(id);
    if (e.reading || e.state.busy) return;
    e.reading = true;
    const version = e.version;
    try {
      const view = await read(id, signal);
      if (version === e.version && !signal?.aborted)
        update(id, {
          view,
          stale: false,
          ...(view.response
            ? { uncertain: false, error: null }
            : e.state.error?.startsWith('Could not refresh')
              ? { error: null }
              : {})
        });
    } catch {
      if (version === e.version && (!signal?.aborted || signal.reason?.name === 'TimeoutError'))
        update(id, {
          stale: true,
          error: 'Could not refresh this request. Responses are disabled until it reconnects.'
        });
    } finally {
      e.reading = false;
    }
  }
  return {
    subscribe(id: string, fn: (s: HITLState) => void) {
      const e = entry(id);
      e.listeners.add(fn);
      fn(e.state);
      return () => {
        e.listeners.delete(fn);
      };
    },
    refresh,
    async poll(id: string, signal: AbortSignal) {
      const delivered = () =>
        !!entry(id).state.view?.response && entry(id).state.view?.transport_state === 'accepted';
      if (delivered()) return false;
      await refresh(id, signal);
      return !delivered();
    },
    async respond(id: string, draft: HITLDraft) {
      const e = entry(id),
        v = e.state.view;
      if (
        !v ||
        e.state.busy ||
        e.state.uncertain ||
        e.state.stale ||
        !v.answerable ||
        !v.writes_enabled ||
        v.response ||
        v.route_request_id !== id
      )
        return;
      let response: HITLResponse;
      try {
        response = buildHITLResponse(v.request.payload, draft);
      } catch (error) {
        update(id, { error: (error as Error).message });
        return;
      }
      e.version++;
      update(id, { busy: true, error: null });
      try {
        const view = await send(id, newId(), response);
        update(id, { view, stale: false });
      } catch (error) {
        const status = (error as { status?: number }).status;
        // 5xx/network errors can follow recording consent. Never invite another decision.
        const uncertain = !status || status >= 500;
        update(id, {
          uncertain,
          stale: true,
          error: uncertain
            ? 'Decision delivery uncertain. Checking the recorded result; do not submit again.'
            : `${(error as Error).message} Refreshing the request.`
        });
      } finally {
        update(id, { busy: false });
        await refresh(id);
      }
    }
  };
}
