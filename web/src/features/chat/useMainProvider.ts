import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState } from 'react';
import { ApiError, fetchClient, keys } from '@/api';
import { POLL_MS } from './useThread';
import {
  createRequestToken,
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

/**
 * The owner's main-provider selection. One explicit write, then a read to confirm what the backend
 * holds. A failed write is never repeated on its own: Retry and Restart run only when the owner
 * asks. A failed read keeps the page locked and is retried on a timer, as a read.
 *
 * `busy` is what the thread does not know itself: offline, a send or stop running, or the thread
 * not read yet.
 */
export function useMainProvider(info: MainThreadInfo | undefined, busy: boolean): MainProviderControl {
  const queryClient = useQueryClient();
  const [selecting, setSelecting] = useState(false);
  const [unconfirmed, setUnconfirmed] = useState(false);
  const [failedWrite, setFailedWrite] = useState<MainProviderSelection | null>(null);
  const [writeNotice, setWriteNotice] = useState<string | null>(null);
  const [tokens] = useState(createRequestToken);
  const inFlight = useRef(false);
  const retryTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(() => () => clearTimeout(retryTimer.current), []);

  const current = info?.provider;

  // Reads the provider state into the thread's cache. A read that a newer request has overtaken
  // is dropped; a failed read keeps the page locked until a later read succeeds.
  const reconcile = async (token: number): Promise<boolean> => {
    try {
      const { data } = await fetchClient.GET('/main-thread/provider');
      if (!data) throw new Error('The provider state could not be read.');
      if (tokens.isLatest(token)) {
        queryClient.setQueryData<MainThreadInfo>(keys.mainThread(), (cached) =>
          cached ? { ...cached, provider: data } : cached
        );
        setUnconfirmed(false);
      }
      return true;
    } catch {
      if (tokens.isLatest(token)) {
        setUnconfirmed(true);
        clearTimeout(retryTimer.current);
        retryTimer.current = setTimeout(() => void reconcile(token), RECONCILE_MS);
      }
      return false;
    }
  };

  // One write, then a read even when the write's outcome is unknown.
  const submit = async (selection: MainProviderSelection) => {
    if (inFlight.current) return;
    inFlight.current = true;
    const token = tokens.next();
    clearTimeout(retryTimer.current);
    setSelecting(true);
    setFailedWrite(null);
    setWriteNotice(null);
    try {
      // Polls already in flight may carry the provider from before the write: drop them.
      await queryClient.cancelQueries({ queryKey: keys.mainThread() });
      try {
        await fetchClient.PUT('/main-thread/provider', { body: selection });
      } catch (error) {
        setWriteNotice(selectionNotice(error));
        // A 409 is a block the backend decided on: retrying the same write would be refused too.
        if (!(error instanceof ApiError && error.status === 409)) setFailedWrite(selection);
      }
      // Drop any poll that started during the write, then read the state the write left.
      await queryClient.cancelQueries({ queryKey: keys.mainThread() });
      await reconcile(token);
    } finally {
      setSelecting(false);
      inFlight.current = false;
    }
  };

  const switchBlocked = providerSwitchBlocked(info, busy || selecting || unconfirmed);
  const notice = [writeNotice, unconfirmed ? UNCONFIRMED_NOTICE : null]
    .filter((text): text is string => text !== null)
    .join(' ');
  const restartTarget = current ? { provider: current.desired_provider, restart_failed: true } : null;

  return {
    selecting,
    unconfirmed,
    switchBlocked,
    notice: notice || null,
    retryOffered: retryOffered(current, failedWrite),
    restartOffered: current?.recoverable === true,
    select: (provider: MainProvider) => {
      if (selectionIsNoop(provider, current)) return;
      void submit({ provider, restart_failed: false });
    },
    retry: () => {
      const target = failedWrite ?? (current ? { provider: current.desired_provider, restart_failed: false } : null);
      if (target) void submit(target);
    },
    restart: () => {
      if (restartTarget) void submit(restartTarget);
    }
  };
}
