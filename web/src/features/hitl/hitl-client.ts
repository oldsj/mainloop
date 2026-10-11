import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { ApiError, fetchClient, keys } from '@/api';
import type {
  HITLMergeDetailSection,
  HITLMergeDetails,
  HITLResponse,
  HITLView
} from '@/lib/contract';
import { createHITLChannel, type HITLState } from '@/lib/hitl';
import { visiblePolling } from '@/lib/visiblePolling';

/** The owner write has no read deadline of its own; 90 s is how long we wait before locking. */
export const RESPOND_TIMEOUT_MS = 90_000;
/** Merge detail pages are capped by the server at 50 entries. */
export const MERGE_DETAIL_PAGE = 50;

export async function readHITL(requestId: string, signal?: AbortSignal): Promise<HITLView> {
  const { data } = await fetchClient.GET('/hitl/{request_id}', {
    params: { path: { request_id: requestId } },
    signal
  });
  if (!data) throw new Error('The server sent an empty response.');
  return data;
}

async function postHITL(
  requestId: string,
  actionId: string,
  response: HITLResponse,
  timeoutMs: number
): Promise<HITLView> {
  const { data } = await fetchClient.POST('/hitl/{request_id}/respond', {
    params: { path: { request_id: requestId } },
    body: { action_id: actionId, response },
    signal: AbortSignal.timeout(timeoutMs)
  });
  if (!data) throw new Error('The server sent an empty response.');
  return data;
}

/**
 * The write path of one channel. A timeout or network failure carries no status, so the channel
 * locks the request as uncertain and re-reads it before anything else can be sent.
 *
 * A 503 also reads as uncertain, because a proxy can return it after the server recorded the
 * decision. The one exception is proven by a re-read: the request is still unanswered and the
 * server reports its write gate off. Then nothing was recorded, and the refusal is shown as a
 * disabled state instead of a lock.
 */
export function createHITLSend(timeoutMs = RESPOND_TIMEOUT_MS) {
  return async (requestId: string, actionId: string, response: HITLResponse) => {
    try {
      return await postHITL(requestId, actionId, response, timeoutMs);
    } catch (error) {
      if ((error as { status?: number }).status === 503) {
        const view = await readHITL(requestId).catch(() => null);
        if (view && !view.response && !view.writes_enabled)
          throw new ApiError('Responding is disabled on the server. Nothing was recorded.', 403);
      }
      throw error;
    }
  };
}

/** One state machine per request id in this browser, shared by every mounted card. */
export const hitlClient = createHITLChannel(readHITL, createHITLSend());

export async function loadMergeDetails(
  requestId: string,
  proposalId: string,
  section: HITLMergeDetailSection,
  cursor: number
): Promise<HITLMergeDetails> {
  const { data } = await fetchClient.GET('/hitl/{request_id}/merge/{proposal_id}/details', {
    params: {
      path: { request_id: requestId, proposal_id: proposalId },
      query: { section, cursor, limit: MERGE_DETAIL_PAGE }
    }
  });
  if (!data) throw new Error('The server sent an empty response.');
  return data;
}

const INITIAL: HITLState = { view: null, busy: false, error: null, uncertain: false, stale: true };

/**
 * The shared state of one request. Mounting starts a visible-tab poll (one read at a time, 4 s
 * apart, stopped once the decision is delivered). The latest view is also written to the query
 * cache under `keys.hitl(id)` so other readers of that key see the same data.
 */
export function useHITL(requestId: string): HITLState {
  const queryClient = useQueryClient();
  const [held, setHeld] = useState<{ id: string; state: HITLState }>({
    id: requestId,
    state: INITIAL
  });
  useEffect(() => {
    const stopSubscription = hitlClient.subscribe(requestId, (state) => {
      setHeld({ id: requestId, state });
      if (state.view) queryClient.setQueryData(keys.hitl(requestId), state.view);
    });
    const stopPolling = visiblePolling().watch(`hitl:${requestId}`, (signal) =>
      hitlClient.poll(requestId, signal)
    );
    return () => {
      stopPolling();
      stopSubscription();
    };
  }, [requestId, queryClient]);
  return held.id === requestId ? held.state : INITIAL;
}
