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
  merge?: {
    pr_url: string;
    head: string;
    base: string;
    protected_matches: string[];
    ci_evidence: string;
  } | null;
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
      })
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
