/** Pure helpers for the merge evidence and the status line. Nothing here calls the backend. */
import type { MergeSummary, VerifiedMergeContext, HITLView } from '@/lib/contract';
import type { Tone } from '@/lib/taskState';

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
    facts.summary_digest
  );
}

/** Facts whose summary is present. mergeContexts only keeps facts that have one. */
export type SummarizedFacts = VerifiedMergeContext & { summary: MergeSummary };
export const hasSummary = (facts: VerifiedMergeContext): facts is SummarizedFacts =>
  facts.summary !== null;

/** Only https links leave the app. A relative or script URL is dropped, not repaired. */
export function safeExternalHref(value: unknown): string | null {
  if (typeof value !== 'string') return null;
  try {
    const parsed = new URL(value);
    return parsed.protocol === 'https:' ? parsed.toString() : null;
  } catch {
    return null;
  }
}

/** Pull request and compare links must be https on github.com; anything else is not a link. */
export function safeGithubHref(value: unknown): string | null {
  const href = safeExternalHref(value);
  return href && new URL(href).hostname === 'github.com' ? href : null;
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
    if (view.transport_state === 'recorded') return 'active';
    return 'bad';
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
