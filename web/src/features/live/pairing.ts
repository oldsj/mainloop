import { useQueries } from '@tanstack/react-query';
import { keys } from '@/api/keys';
import { hitlClient, readHITL, respondHITL } from '@/features/hitl/hitl-client';
import { mergeContexts, parseHITL, requestTools, type HITLState } from '@/lib/hitl';
import type { HITLView } from '@/lib/contract';

/** While a prompt waits for its request, the session's requests are re-read this often. */
export const PAIR_POLL_MS = 1000;

/** The session request that holds a live permission prompt, and the call in it. */
export interface PermPair {
  requestId: string;
  toolId: string;
}

/**
 * Finds the session request behind a permission prompt the runner shows live. A prompt in a turn
 * Mainloop sent is also the A2A task's input-required request, which kagent raises with the
 * runner's permission id as the call's id and the SDK's tool_use_id as its call id. Only a direct,
 * unanswered request with that one call pairs: a merge review needs its card, and a request with
 * several calls is answered as a whole there.
 */
export function pairOf(
  views: readonly (HITLView | undefined)[],
  perm: { id: string; toolUseID?: string }
): PermPair | undefined {
  for (const view of views) {
    if (!view || view.response || !view.answerable) continue;
    const p = parseHITL(view.request.payload);
    if (!p || p.type !== 'tool_approval_request' || p.nested) continue;
    const tools = requestTools(p);
    if (tools.length !== 1 || mergeContexts(view).length) continue;
    const [tool] = tools;
    if (tool && (tool.id === perm.id || (perm.toolUseID && tool.call_id === perm.toolUseID))) {
      return { requestId: view.request.id, toolId: tool.id };
    }
  }
  return undefined;
}

/** The session's pending requests, read in full so prompts can be paired with them. */
export function usePendingViews(requestIds: readonly string[]): (HITLView | undefined)[] {
  return useQueries({
    queries: requestIds.map((id) => ({
      queryKey: keys.hitl(id),
      queryFn: ({ signal }: { signal: AbortSignal }) => readHITL(id, signal),
      staleTime: 2000
    }))
  }).map((q) => q.data);
}

/**
 * Answers a paired prompt through Mainloop's request API, which then answers the A2A task. The
 * request's channel is the one its card uses, so it is read first; an answer the server did not
 * record rejects with the channel's reason.
 */
export async function answerPair(
  pair: PermPair,
  approve: boolean,
  sessionId: string
): Promise<void> {
  await hitlClient.refresh(pair.requestId);
  await respondHITL(
    pair.requestId,
    { decisions: { [pair.toolId]: approve ? 'approve' : 'reject' }, reasons: {}, answers: [] },
    sessionId
  );
  let state: HITLState | undefined;
  hitlClient.subscribe(pair.requestId, (s) => {
    state = s;
  })();
  if (!state?.view?.response)
    throw new Error(state?.error ?? 'Mainloop did not record the answer.');
}
