import { useQueryClient, type QueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState } from 'react';
import { ApiError, fetchClient, keys } from '@/api';
import { POLL_MS } from './useThread';
import {
  createRequestToken,
  providerRetryBlocked,
  providerSwitchBlocked,
  retryOffered,
  selectionIsNoop,
  selectionNotice,
  UNCONFIRMED_NOTICE,
  type MainProviderControl
} from './mainProvider';
import type { MainProvider, MainProviderSelection, MainThreadInfo } from './types';

/** How soon a failed read of the provider state is tried again. Reads only, never the write. */
export const RECONCILE_MS = POLL_MS / 4;

/** What a provider read changes on the page: the thread cache, the lock, the token and the timer. */
interface ProviderRead {
  queryClient: QueryClient;
  tokens: ReturnType<typeof createRequestToken>;
  timer: { current: ReturnType<typeof setTimeout> | undefined };
  setUnconfirmed: (unconfirmed: boolean) => void;
}

/**
 * Reads the provider state into the thread's cache. A read that a newer request has overtaken is
 * dropped. A failed read keeps the page locked and is read again on a timer; a read that still says
 * pending is read again on the poll interval. These are GETs only: the write is never repeated here.
 */
async function readProvider(read: ProviderRead, token: number): Promise<void> {
  let again: number | null = null;
  try {
    const { data } = await fetchClient.GET('/main-thread/provider');
    if (!data) throw new Error('The provider state could not be read.');
    if (!read.tokens.isLatest(token)) return;
    read.queryClient.setQueryData<MainThreadInfo>(keys.mainThread(), (cached) =>
      cached ? { ...cached, provider: data } : cached
    );
    read.setUnconfirmed(false);
    again = data.status === 'pending' ? POLL_MS : null;
  } catch {
    if (!read.tokens.isLatest(token)) return;
    read.setUnconfirmed(true);
    again = RECONCILE_MS;
  }
  if (again === null) return;
  clearTimeout(read.timer.current);
  read.timer.current = setTimeout(() => void readProvider(read, token), again);
}

/**
 * The owner's main-provider selection. One explicit write, then a read to confirm what the backend
 * holds. A failed write is never repeated on its own: Retry and Restart run only when the owner
 * asks. A failed read keeps the page locked and is retried on a timer, as a read. A pending switch
 * is read again on the poll interval until it settles, and sends no write.
 *
 * `busy` is what the thread does not know itself: offline, a send or stop running, or the thread
 * not read yet.
 */
export function useMainProvider(
  info: MainThreadInfo | undefined,
  busy: boolean
): MainProviderControl {
  const queryClient = useQueryClient();
  const [selecting, setSelecting] = useState(false);
  const [unconfirmed, setUnconfirmed] = useState(false);
  const [failedWrite, setFailedWrite] = useState<MainProviderSelection | null>(null);
  const [writeNotice, setWriteNotice] = useState<string | null>(null);
  const [tokens] = useState(createRequestToken);
  const inFlight = useRef(false);
  // The one scheduled read: a retry after a failed read, or the next poll while pending.
  const readTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  // Leaving the page drops the scheduled read and any read still in flight.
  useEffect(
    () => () => {
      clearTimeout(readTimer.current);
      tokens.next();
    },
    [tokens]
  );

  const current = info?.provider;
  const pending = current?.status === 'pending';

  // While the main is pending, its state is read on the poll interval until it settles. Each run
  // takes a new token, so a read left over from an earlier run is dropped, not applied late. No
  // cleanup here: a read still scheduled ends itself once the state settles, and unmount drops it.
  useEffect(() => {
    if (!pending || selecting) return;
    clearTimeout(readTimer.current);
    void readProvider({ queryClient, tokens, timer: readTimer, setUnconfirmed }, tokens.next());
  }, [pending, selecting, queryClient, tokens]);

  // One write, then a read even when the write's outcome is unknown.
  const submit = async (selection: MainProviderSelection) => {
    if (inFlight.current) return;
    inFlight.current = true;
    const token = tokens.next();
    clearTimeout(readTimer.current);
    setSelecting(true);
    setFailedWrite(null);
    setWriteNotice(null);
    try {
      // Polls already in flight may carry the provider from before the write: drop them.
      await queryClient.cancelQueries({ queryKey: keys.mainThread() });
      try {
        await fetchClient.PUT('/main-thread/provider', { body: selection });
      } catch (error) {
        setWriteNotice(selectionNotice(error, selection.provider));
        // A 409 is a block the backend decided on: retrying the same write would be refused too.
        if (!(error instanceof ApiError && error.status === 409)) setFailedWrite(selection);
      }
      // Drop any poll that started during the write, then read the state the write left.
      await queryClient.cancelQueries({ queryKey: keys.mainThread() });
      await readProvider({ queryClient, tokens, timer: readTimer, setUnconfirmed }, token);
    } finally {
      setSelecting(false);
      inFlight.current = false;
    }
  };

  const held = busy || selecting || unconfirmed;
  const switchBlocked = providerSwitchBlocked(info, held);
  const retryBlocked = providerRetryBlocked(info, held);
  const notice = [writeNotice, unconfirmed ? UNCONFIRMED_NOTICE : null]
    .filter((text): text is string => text !== null)
    .join(' ');
  const desiredWrite = current
    ? { provider: current.desired_provider, restart_failed: false }
    : null;
  const restartTarget = current
    ? { provider: current.desired_provider, restart_failed: true }
    : null;

  return {
    selecting,
    unconfirmed,
    switchBlocked,
    retryBlocked,
    notice: notice || null,
    retryOffered: retryOffered(current, failedWrite),
    restartOffered: current?.recoverable === true,
    select: (provider: MainProvider) => {
      if (selectionIsNoop(provider, current)) return;
      void submit({ provider, restart_failed: false });
    },
    retry: () => {
      // A pending switch is repaired by repeating its own write, which is the desired provider.
      const target = pending ? desiredWrite : (failedWrite ?? desiredWrite);
      if (target) void submit(target);
    },
    restart: () => {
      if (restartTarget) void submit(restartTarget);
    }
  };
}
