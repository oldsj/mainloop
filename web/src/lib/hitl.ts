/** Display and response contracts. Only the server decides whether a request is answerable. */
import { z } from 'zod';
import type { components } from '@/api';
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
import { contractEnum } from './contractEnum';

/** A request as the server sends it: a tool approval or a question. parseHITL checks it. */
export type HITLPayload = ToolApprovalRequest | AskUserRequest;

type Schemas = components['schemas'];

/*
 * Each schema is declared against the generated type it names, so tsc fails when the contract drifts.
 * The checks here are the ones the approver needs: bounded text, digests, unique tool ids and one
 * nested tool per question. A null passes only where the generated type allows null.
 */
const record = (v: unknown): v is Record<string, unknown> =>
  !!v && typeof v === 'object' && !Array.isArray(v);
const bounded = (max = 2048) => z.string().min(1).max(max);
const count = z.number().int();
const positive = count.positive();
const digest = z.string().regex(/^[a-f0-9]{64}$/);
const sha = z.string().regex(/^[a-f0-9]{40}$/);
const repo = z.string().regex(/^[\w.-]+\/[\w.-]+$/);
const choiceList = z.array(z.string().max(4096)).max(100);

const toolSchema = z.looseObject({
  id: bounded(),
  call_id: bounded(),
  name: bounded(),
  args: z.record(z.string(), z.unknown())
}) satisfies z.ZodType<HITLTool>;

// Tool ids key the decisions, so a repeated id would let one decision answer two calls.
const toolsSchema = z
  .array(toolSchema)
  .min(1)
  .max(100)
  .refine(
    (tools) => new Set(tools.map((t) => t.id)).size === tools.length,
    'Tool ids must be unique'
  );

const nestedSchema = z.looseObject({
  subagent_name: z.string(),
  task_id: bounded(),
  context_id: bounded(),
  tools: toolsSchema
}) satisfies z.ZodType<NestedHITLRequest>;

const questionSchema = z.looseObject({
  question: bounded(8192),
  multiple: z.boolean(),
  choices: choiceList.nullable()
}) satisfies z.ZodType<HITLQuestion>;

const approvalSchema = z.looseObject({
  type: z.literal('tool_approval_request'),
  hint: z.string().max(4096),
  tools: toolsSchema,
  nested: nestedSchema.nullable()
}) satisfies z.ZodType<ToolApprovalRequest>;

// A question's nested request names exactly one tool: the call the owner is answering for.
const askUserSchema = z
  .looseObject({
    type: z.literal('ask_user_request'),
    id: bounded(),
    questions: z.array(questionSchema).min(1).max(100),
    nested: nestedSchema.nullable()
  })
  .refine(
    (v) => v.nested === null || v.nested.tools.length === 1,
    'A nested request names one tool'
  ) satisfies z.ZodType<AskUserRequest>;

const payloadSchema = z.union([approvalSchema, askUserSchema]);

const reasonSchema = z.discriminatedUnion('type', [
  z.looseObject({ type: z.literal('project_policy') }),
  z.looseObject({ type: z.literal('protected_path'), glob: z.string(), path: z.string() })
]) satisfies z.ZodType<Schemas['MergeApprovalReason']>;

const ciSchema = z.looseObject({
  complete: z.boolean(),
  captured_at: z.string().nullable(),
  result_count: count,
  passed_count: count,
  failed_count: count,
  pending_count: count,
  green_at_preparation: z.boolean(),
  inventory_digest: digest,
  ignored_suite_count: count.optional()
}) satisfies z.ZodType<Schemas['MergeCISummary']>;

const mappingSchema = z.looseObject({
  template_name: bounded(),
  provider: z.enum(
    contractEnum<Schemas['TemplateMappingEvidence']['provider']>()(['claude', 'codex'])
  ),
  config_digest: z.string()
}) satisfies z.ZodType<Schemas['TemplateMappingEvidence']>;

/** The shape the generated type allows. Evidence is judged separately by isEvidenceSummary. */
const summarySchema = z.looseObject({
  version: z.literal(1),
  proposal_id: bounded(),
  repository: repo.nullable(),
  pr_number: positive.nullable(),
  title: z.string().nullable(),
  description_excerpt: z.string(),
  description_truncated: z.boolean(),
  description_digest: digest.nullable(),
  description_length: count.nullable(),
  file_count: count.nullable(),
  additions: count.nullable(),
  deletions: count.nullable(),
  paths_digest: digest.nullable(),
  paths_preview: z.array(z.string()),
  paths_preview_truncated: z.boolean(),
  protected_matches: z.array(z.string()),
  approval_reasons: z.array(reasonSchema),
  policy: z
    .enum(contractEnum<NonNullable<MergeSummary['policy']>>()(['auto', 'approval']))
    .nullable(),
  policy_version: count.nullable(),
  globs_version: count.nullable(),
  head: bounded().nullable(),
  head_sha: sha.nullable(),
  base: bounded().nullable(),
  base_sha: sha.nullable(),
  ci: ciSchema.nullable(),
  mapping_evidence: mappingSchema.nullable(),
  availability: z.enum(contractEnum<MergeSummary['availability']>()(['ready', 'unavailable'])),
  unavailable_reasons: z.array(z.string()),
  merge_file_count: count.optional(),
  merge_only_file_count: count.optional(),
  merge_paths_digest: digest.nullable().optional()
}) satisfies z.ZodType<MergeSummary>;

const contextSchema = z.looseObject({
  tool_id: bounded().nullable(),
  proposal_id: bounded().nullable(),
  summary: summarySchema.nullable(),
  summary_digest: digest.nullable(),
  availability: z.enum(
    contractEnum<VerifiedMergeContext['availability']>()(['ready', 'unavailable', 'stale'])
  ),
  freshness_reason: z.string().nullable(),
  details_url: z.string().nullable(),
  pr_url: z.string().optional(),
  compare_url: z.string().optional(),
  repository: repo.nullable(),
  pr_number: positive.nullable(),
  head: bounded().nullable(),
  head_sha: sha.nullable(),
  base: bounded().nullable(),
  base_sha: sha.nullable(),
  protected_matches: z.array(z.string()),
  stale: z.boolean(),
  state: z.string().nullable(),
  deadline: z.string().nullable()
}) satisfies z.ZodType<VerifiedMergeContext>;

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
    const parsed = contextSchema.safeParse(v);
    const facts = parsed.success && isEvidence(parsed.data) ? parsed.data : null;
    return [{ toolId: tool.id, facts }];
  });
}
export function parseHITL(payload: unknown): HITLPayload | null {
  try {
    if (new TextEncoder().encode(JSON.stringify(payload)).length > 131072) return null;
  } catch {
    return null;
  }
  const parsed = payloadSchema.safeParse(payload);
  return parsed.success ? parsed.data : null;
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
