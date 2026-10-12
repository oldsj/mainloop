/**
 * Sanitized HITL fixtures in the shape of the generated contract (`api/schema.gen.ts`). IDs and
 * digests are invented. Each factory returns a complete generated type, so a contract change
 * fails tsc here rather than in a test that happens to read the missing field.
 */
import type {
  DecisionReceipt,
  HITLView,
  MergeSummary,
  ToolApprovalRequest,
  VerifiedMergeContext
} from '@/lib/contract';

export function approvalRequest(overrides: Partial<ToolApprovalRequest> = {}): ToolApprovalRequest {
  return {
    type: 'tool_approval_request',
    hint: '',
    tools: [
      { id: 'a', call_id: 'native-a', name: 'tool.a', args: { x: '<script>' } },
      { id: 'b', call_id: 'native-b', name: 'tool.b', args: {} }
    ],
    nested: null,
    unknown: { retained: true },
    ...overrides
  };
}

export function hitlView(id = 'request', overrides: Partial<HITLView> = {}): HITLView {
  return {
    request: {
      id,
      owner_id: 'owner',
      outer: {
        gateway: 'gateway',
        endpoint: 'endpoint',
        runtime_session_id: 'session',
        context_id: 'context',
        task_id: 'task',
        status_message_id: 'status-message',
        request_hash: 'hash'
      },
      payload: approvalRequest(),
      leaves: [],
      associations: [],
      availability: 'pending',
      unavailable_reason: null
    },
    response: null,
    transport_state: null,
    route_request_id: id,
    answerable: true,
    unavailable_reason: null,
    context: {},
    writes_enabled: true,
    merge: null,
    ...overrides
  };
}

/** The receipt the server keeps once a decision is recorded. Its response is the output form. */
export function decisionReceipt(request: HITLView['request']): DecisionReceipt {
  return {
    action_id: 'action',
    owner_id: request.owner_id,
    request_hash: request.outer.request_hash,
    request: request.payload,
    response: {
      type: 'tool_approval_response',
      approvals: [
        { id: 'a', approved: true, rejection_reason: null },
        { id: 'b', approved: false, rejection_reason: 'Keep tests' }
      ],
      reviewed_context: {}
    },
    outer: request.outer,
    calls: [],
    associations: [],
    outbound_message_id: 'outbound'
  };
}

export function mergeSummary(overrides: Partial<MergeSummary> = {}): MergeSummary {
  return {
    version: 1,
    proposal_id: 'proposal-1',
    repository: 'owner/repo',
    pr_number: 7,
    title: 'Add retry button',
    description_excerpt: 'Adds a retry button.',
    description_truncated: false,
    description_digest: 'd'.repeat(64),
    description_length: 20,
    file_count: 2,
    additions: 10,
    deletions: 1,
    paths_digest: 'e'.repeat(64),
    paths_preview: ['src/a.ts', 'src/b.ts'],
    paths_preview_truncated: false,
    protected_matches: [],
    approval_reasons: [{ type: 'project_policy' }],
    policy: 'approval',
    policy_version: 1,
    globs_version: 1,
    head: 'feature',
    head_sha: 'b'.repeat(40),
    base: 'main',
    base_sha: 'c'.repeat(40),
    ci: null,
    mapping_evidence: null,
    availability: 'ready',
    unavailable_reasons: [],
    ...overrides
  };
}

export function mergeContext(overrides: Partial<VerifiedMergeContext> = {}): VerifiedMergeContext {
  return {
    tool_id: 'a',
    proposal_id: 'proposal-1',
    summary: mergeSummary(),
    summary_digest: 'a'.repeat(64),
    availability: 'ready',
    freshness_reason: null,
    details_url: null,
    pr_url: 'https://github.com/owner/repo/pull/7',
    repository: 'owner/repo',
    pr_number: 7,
    head: 'feature',
    head_sha: 'b'.repeat(40),
    base: 'main',
    base_sha: 'c'.repeat(40),
    protected_matches: [],
    stale: false,
    state: null,
    deadline: null,
    ...overrides
  };
}
