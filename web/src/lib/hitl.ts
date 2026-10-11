/** Display and response contracts. Only the server decides whether a request is answerable. */
import type {
  AskUserRequest,
  HITLQuestion,
  HITLResponse,
  HITLTool,
  HITLView,
  MergeSummary,
  NestedHITLRequest,
  ToolApprovalRequest,
  VerifiedMergeContext
} from './contract';

/** A request as the server sends it: a tool approval or a question. parseHITL checks it. */
export type HITLPayload = ToolApprovalRequest | AskUserRequest;

type MergeCI = NonNullable<MergeSummary['ci']>;
type MergeMapping = NonNullable<MergeSummary['mapping_evidence']>;
type MergeReason = MergeSummary['approval_reasons'][number];

// Each check mirrors one generated type. A null passes only where the schema allows null.
const record = (v: unknown): v is Record<string, unknown> =>
  !!v && typeof v === 'object' && !Array.isArray(v);
const text = (v: unknown): v is string => typeof v === 'string';
const bounded = (v: unknown, max = 2048): v is string =>
  typeof v === 'string' && v.length > 0 && v.length <= max;
const count = (v: unknown): v is number => typeof v === 'number' && Number.isSafeInteger(v);
const positive = (v: unknown): v is number => count(v) && v > 0;
const digest = (v: unknown): v is string => typeof v === 'string' && /^[a-f0-9]{64}$/.test(v);
const sha = (v: unknown): v is string => typeof v === 'string' && /^[a-f0-9]{40}$/.test(v);
const repo = (v: unknown): v is string => typeof v === 'string' && /^[\w.-]+\/[\w.-]+$/.test(v);
const strings = (v: unknown): v is string[] => Array.isArray(v) && v.every(text);
const choiceList = (v: unknown): v is string[] =>
  Array.isArray(v) && v.length <= 100 && v.every((c) => typeof c === 'string' && c.length <= 4096);
const nullOr = <T>(check: (v: unknown) => v is T, v: unknown): v is T | null =>
  v === null || check(v);

const isTool = (t: unknown): t is HITLTool =>
  record(t) && bounded(t.id) && bounded(t.call_id) && bounded(t.name) && record(t.args);
const isTools = (v: unknown): v is HITLTool[] =>
  Array.isArray(v) &&
  v.length > 0 &&
  v.length <= 100 &&
  v.every(isTool) &&
  new Set(v.map((t) => t.id)).size === v.length;
const isNested = (v: unknown): v is NestedHITLRequest =>
  record(v) &&
  text(v.subagent_name) &&
  bounded(v.task_id) &&
  bounded(v.context_id) &&
  isTools(v.tools);
const isQuestion = (q: unknown): q is HITLQuestion =>
  record(q) &&
  bounded(q.question, 8192) &&
  typeof q.multiple === 'boolean' &&
  nullOr(choiceList, q.choices);
const isApproval = (v: unknown): v is ToolApprovalRequest =>
  record(v) &&
  v.type === 'tool_approval_request' &&
  text(v.hint) &&
  v.hint.length <= 4096 &&
  isTools(v.tools) &&
  nullOr(isNested, v.nested);
const isAskUser = (v: unknown): v is AskUserRequest =>
  record(v) &&
  v.type === 'ask_user_request' &&
  bounded(v.id) &&
  Array.isArray(v.questions) &&
  v.questions.length > 0 &&
  v.questions.length <= 100 &&
  v.questions.every(isQuestion) &&
  (v.nested === null || (isNested(v.nested) && v.nested.tools.length === 1));

const isReason = (v: unknown): v is MergeReason =>
  record(v) &&
  (v.type === 'project_policy' || (v.type === 'protected_path' && text(v.glob) && text(v.path)));
const isCI = (v: unknown): v is MergeCI =>
  record(v) &&
  typeof v.complete === 'boolean' &&
  nullOr(text, v.captured_at) &&
  count(v.result_count) &&
  count(v.passed_count) &&
  count(v.failed_count) &&
  count(v.pending_count) &&
  typeof v.green_at_preparation === 'boolean' &&
  digest(v.inventory_digest) &&
  (v.ignored_suite_count === undefined || count(v.ignored_suite_count));
const isMapping = (v: unknown): v is MergeMapping =>
  record(v) &&
  bounded(v.template_name) &&
  (v.provider === 'claude' || v.provider === 'codex') &&
  text(v.config_digest);
const policy = (v: unknown): v is 'auto' | 'approval' => v === 'auto' || v === 'approval';
/** The shape the generated type allows. Evidence is judged separately by isEvidenceSummary. */
const isSummary = (v: unknown): v is MergeSummary =>
  record(v) &&
  v.version === 1 &&
  bounded(v.proposal_id) &&
  nullOr(repo, v.repository) &&
  nullOr(positive, v.pr_number) &&
  nullOr(text, v.title) &&
  text(v.description_excerpt) &&
  typeof v.description_truncated === 'boolean' &&
  nullOr(digest, v.description_digest) &&
  nullOr(count, v.description_length) &&
  nullOr(count, v.file_count) &&
  nullOr(count, v.additions) &&
  nullOr(count, v.deletions) &&
  nullOr(digest, v.paths_digest) &&
  strings(v.paths_preview) &&
  typeof v.paths_preview_truncated === 'boolean' &&
  strings(v.protected_matches) &&
  Array.isArray(v.approval_reasons) &&
  v.approval_reasons.every(isReason) &&
  nullOr(policy, v.policy) &&
  nullOr(count, v.policy_version) &&
  nullOr(count, v.globs_version) &&
  nullOr(bounded, v.head) &&
  nullOr(sha, v.head_sha) &&
  nullOr(bounded, v.base) &&
  nullOr(sha, v.base_sha) &&
  nullOr(isCI, v.ci) &&
  nullOr(isMapping, v.mapping_evidence) &&
  (v.availability === 'ready' || v.availability === 'unavailable') &&
  strings(v.unavailable_reasons) &&
  (v.merge_file_count === undefined || count(v.merge_file_count)) &&
  (v.merge_only_file_count === undefined || count(v.merge_only_file_count)) &&
  (v.merge_paths_digest === undefined || nullOr(digest, v.merge_paths_digest));
const isContext = (v: unknown): v is VerifiedMergeContext =>
  record(v) &&
  nullOr(bounded, v.tool_id) &&
  nullOr(bounded, v.proposal_id) &&
  nullOr(isSummary, v.summary) &&
  nullOr(digest, v.summary_digest) &&
  (v.availability === 'ready' || v.availability === 'unavailable' || v.availability === 'stale') &&
  nullOr(text, v.freshness_reason) &&
  nullOr(text, v.details_url) &&
  (v.pr_url === undefined || text(v.pr_url)) &&
  (v.compare_url === undefined || text(v.compare_url)) &&
  nullOr(repo, v.repository) &&
  nullOr(positive, v.pr_number) &&
  nullOr(bounded, v.head) &&
  nullOr(sha, v.head_sha) &&
  nullOr(bounded, v.base) &&
  nullOr(sha, v.base_sha) &&
  strings(v.protected_matches) &&
  typeof v.stale === 'boolean' &&
  nullOr(text, v.state) &&
  nullOr(text, v.deadline);

/**
 * Approval reads these summary fields. A null in any of them leaves the approver without proof.
 * ci, mapping_evidence and the merge_* counts may be null by design, so they are not required here.
 */
function isEvidenceSummary(s: MergeSummary): boolean {
  return (
    s.repository !== null &&
    s.pr_number !== null &&
    s.title !== null &&
    s.description_digest !== null &&
    s.description_length !== null &&
    s.file_count !== null &&
    s.additions !== null &&
    s.deletions !== null &&
    s.paths_digest !== null &&
    s.policy !== null &&
    s.policy_version !== null &&
    s.globs_version !== null &&
    s.head !== null &&
    s.head_sha !== null &&
    s.base !== null &&
    s.base_sha !== null
  );
}
function isEvidence(v: VerifiedMergeContext): boolean {
  const s = v.summary;
  return (
    v.tool_id !== null &&
    v.proposal_id !== null &&
    s !== null &&
    s.proposal_id === v.proposal_id &&
    v.summary_digest !== null &&
    v.repository !== null &&
    v.pr_number !== null &&
    v.head !== null &&
    v.head_sha !== null &&
    v.base !== null &&
    v.base_sha !== null &&
    isEvidenceSummary(s)
  );
}

export interface HITLDraft {
  decisions: Record<string, 'approve' | 'reject'>;
  reasons: Record<string, string>;
  answers: string[][];
  reviewedContext?: Record<string, string>;
}
/** Missing, malformed or ambiguous resolver evidence must not look verified. */
export function mergeContexts(view: HITLView) {
  const payload = parseHITL(view.request.payload);
  const entries: unknown[] = Array.isArray(view.merge) ? view.merge : [];
  return (payload ? requestTools(payload) : []).flatMap((tool) => {
    const matches = entries.filter((v) => record(v) && v.tool_id === tool.id);
    // A public name is only a hint to show an unavailable notice, never evidence.
    if (!matches.length && !tool.name.endsWith('merge_pull_request_with_approval')) return [];
    const v: unknown = matches.length === 1 ? matches[0] : null;
    const facts = isContext(v) && isEvidence(v) ? v : null;
    return [{ toolId: tool.id, facts }];
  });
}
export function parseHITL(payload: unknown): HITLPayload | null {
  if (!record(payload)) return null;
  try {
    if (new TextEncoder().encode(JSON.stringify(payload)).length > 131072) return null;
  } catch {
    return null;
  }
  if (isApproval(payload)) return payload;
  if (isAskUser(payload)) return payload;
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
    const { choices } = q;
    if (choices?.length && answer.some((a) => !choices.includes(a)))
      throw new Error('Choose one of the offered answers.');
    return { answer: [...answer] };
  });
  const [nestedTool] = p.nested?.tools ?? [];
  return { type: 'ask_user_response', id: nestedTool?.id ?? p.id, answers };
}
export function reasonNotice(provider?: string | null): string {
  return provider === 'codex'
    ? 'Reason saved; runtime receives rejection only.'
    : provider === 'claude'
      ? 'The rejection reason is included in Claude’s denial message.'
      : 'Reason saved; delivery to this runtime is not verified.';
}
export function hitlStatus(v: HITLView): string {
  if (v.response) {
    const labels: Record<string, string> = {
      accepted: 'Decision delivered',
      recorded: 'Decision recorded; awaiting delivery',
      sending: 'Decision delivery uncertain',
      uncertain: 'Decision delivery uncertain',
      rejected_transport: 'Decision destination unavailable'
    };
    const key = v.transport_state ?? '';
    return (Object.hasOwn(labels, key) && labels[key]) || 'Decision recorded';
  }
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
          error:
            e.state.error && !e.state.error.startsWith('Could not refresh')
              ? e.state.error
              : 'Could not refresh this request. Responses are disabled until it reconnects.'
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
