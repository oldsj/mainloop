/** Pure helpers for the merge evidence and the status line. Nothing here calls the backend. */
import type { MergeSummary, VerifiedMergeContext, HITLView } from '@/lib/contract';
import type { Tone } from '@/lib/taskState';
import { githubUrl } from '@/lib/url';

/** Facts that may back an Approve: ready, current, and carrying the digest the reviewer saw. */
export type ReviewableFacts = VerifiedMergeContext & {
  summary: MergeSummary;
  summary_digest: string;
};

export function isReviewable(facts: VerifiedMergeContext | null): facts is ReviewableFacts {
  return !!(
    facts &&
    facts.availability === 'ready' &&
    !facts.stale &&
    facts.summary?.availability === 'ready' &&
    facts.summary_digest &&
    summaryMatchesContext(facts)
  );
}

/**
 * The digest covers the summary, so the summary's repository and PR number are what the reviewer
 * approves. A context that names a different pull request is not evidence for this summary.
 */
export function summaryMatchesContext(facts: VerifiedMergeContext): boolean {
  const { summary } = facts;
  return (
    !!summary && summary.repository === facts.repository && summary.pr_number === facts.pr_number
  );
}

/** Facts whose summary is present and agrees with the context. */
export type SummarizedFacts = VerifiedMergeContext & { summary: MergeSummary };
export const hasSummary = (facts: VerifiedMergeContext): facts is SummarizedFacts =>
  facts.summary !== null && summaryMatchesContext(facts);

/** The key that identifies the evidence on screen; it changes when the summary does. */
export const evidenceKey = (facts: VerifiedMergeContext | null): string =>
  facts ? `${facts.proposal_id ?? ''}:${facts.summary_digest ?? ''}` : '';

/**
 * A PR link counts only when its path is exactly `/{repository}/pull/{number}`. The link returned
 * is rebuilt from those parts, so no query, fragment or userinfo from the server reaches the page.
 */
export function prLinkFor(
  value: unknown,
  repository: string | null,
  prNumber: number | null
): string | null {
  const href = githubUrl(value);
  if (!href || !repository || prNumber === null) return null;
  const path = `/${repository}/pull/${prNumber}`;
  return new URL(href).pathname === path ? `https://github.com${path}` : null;
}

/**
 * A compare link counts only under `/{repository}/compare/`, so it cannot open another repository's
 * diff. Like the PR link, it is rebuilt from its path alone.
 */
export function compareLinkFor(value: unknown, repository: string | null): string | null {
  const href = githubUrl(value);
  if (!href || !repository) return null;
  const prefix = `/${repository}/compare/`;
  const { pathname } = new URL(href);
  return pathname.startsWith(prefix) && pathname.length > prefix.length
    ? `https://github.com${pathname}`
    : null;
}

/** The recorded response must have the shape the receipt reads; anything else is a problem. */
function receiptProblem(receipt: unknown): string | null {
  if (receipt === null || receipt === undefined) return null;
  const response = (receipt as { response?: unknown }).response;
  if (!response || typeof response !== 'object') return 'the recorded response is missing';
  const { type, approvals, answers } = response as {
    type?: unknown;
    approvals?: unknown;
    answers?: unknown;
  };
  if (type === 'tool_approval_response') {
    return Array.isArray(approvals) && approvals.every((a) => !!a && typeof a === 'object')
      ? null
      : 'the recorded approvals are malformed';
  }
  if (type === 'ask_user_response') {
    return Array.isArray(answers) &&
      answers.every(
        (a) => !!a && typeof a === 'object' && Array.isArray((a as { answer?: unknown }).answer)
      )
      ? null
      : 'the recorded answers are malformed';
  }
  return 'the recorded response has an unknown type';
}

/** A view missing what the renderer reads is a contract violation: show an error, never throw. */
export function viewProblem(view: HITLView): string | null {
  const request: unknown = view.request;
  if (!request || typeof request !== 'object') return 'the request is missing';
  const { outer, payload } = request as { outer?: unknown; payload?: unknown };
  if (!outer || typeof outer !== 'object') return 'the request has no session identity';
  if (payload === undefined || payload === null) return 'the request has no payload';
  return receiptProblem(view.response);
}

const NOTE_LIMIT = 500;
const note = (v: unknown): string[] =>
  typeof v === 'string' && v.trim() ? [v.trim().slice(0, NOTE_LIMIT)] : [];

/**
 * What the server said about evidence that did not pass the checks, as text only. These strings
 * explain why evidence is missing; they never enable an approval. An ambiguous entry (none, or
 * several for one call) yields nothing.
 */
export function unavailableNotes(view: HITLView, toolId: string): string[] {
  const entries: unknown[] = Array.isArray(view.merge) ? view.merge : [];
  const matches = entries.filter(
    (e): e is Record<string, unknown> =>
      !!e && typeof e === 'object' && (e as Record<string, unknown>).tool_id === toolId
  );
  if (matches.length !== 1) return [];
  const entry = matches[0];
  if (!entry) return [];
  const summary = entry.summary;
  const reasons =
    summary &&
    typeof summary === 'object' &&
    Array.isArray((summary as MergeSummary).unavailable_reasons)
      ? (summary as MergeSummary).unavailable_reasons.flatMap(note)
      : [];
  return [...new Set([...note(entry.freshness_reason), ...reasons])].slice(0, 5);
}

/** Tone of the status line. The words come from hitlStatus; this only picks the colour. */
export function statusTone(view: HITLView): Tone {
  if (view.response) {
    if (view.transport_state === 'accepted') return 'ok';
    // Only the states hitlStatus words as a problem are `bad`; null or unknown reads "Decision recorded".
    if (view.transport_state === 'recorded') return 'active';
    if (
      view.transport_state === 'sending' ||
      view.transport_state === 'uncertain' ||
      view.transport_state === 'rejected_transport'
    )
      return 'bad';
    return 'active';
  }
  if (view.route_request_id !== view.request.id) return 'muted';
  if (view.request.availability === 'stale') return 'attention';
  if (!view.answerable || !view.writes_enabled) return 'muted';
  return 'attention';
}

/** Display bound for agent-supplied JSON. The server caps a request at 128 KiB. */
export function clipText(text: string, limit = 131072): string {
  return text.length > limit ? `${text.slice(0, limit)}\n… (truncated)` : text;
}
