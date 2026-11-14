/**
 * The one status a session's row shows. The thread (delivery ledger and reachability) and the live
 * view (its channel and the runner's turn) each know part of it; `barStatus` puts them in one
 * order, so the row never says "ready" beside "asleep".
 */
import type { ThreadStatus } from './delivery';

/** What the live view knows: the runner's turn while attached, else what the channel is doing. */
export type LiveStatus =
  | 'needs you'
  | 'starting'
  | 'working'
  | 'ready'
  | 'asleep'
  | 'paused'
  | 'waking…'
  | 'connecting…'
  | 'reconnecting…'
  | 'unavailable';

export type BarStatus =
  | LiveStatus
  | 'unreachable'
  | 'last message failed'
  | 'last message unconfirmed'
  | 'idle';

/** The words for a thread status. A failed or unconfirmed last delivery is never called ready. */
export function threadWords(status: ThreadStatus): BarStatus {
  if (status === 'failed') return 'last message failed';
  if (status === 'unconfirmed') return 'last message unconfirmed';
  return status;
}

/**
 * Most urgent first: the backend cannot be reached; the owner is asked; a turn is running; the last
 * message failed or is unconfirmed; then whatever the live channel says (asleep, connecting, ready).
 * Without a live view the thread's own status stands.
 */
export function barStatus(thread: ThreadStatus | null, live: LiveStatus | null): BarStatus {
  if (thread === 'unreachable') return 'unreachable';
  if (live === null) return thread ? threadWords(thread) : 'idle';
  if (live === 'needs you' || live === 'working' || live === 'starting') return live;
  if (thread === 'failed' || thread === 'unconfirmed' || thread === 'working')
    return threadWords(thread);
  return live;
}

const DOT: Record<BarStatus, string> = {
  'needs you': 'bg-status-attention',
  'last message unconfirmed': 'bg-status-attention',
  working: 'bg-status-working animate-pulse',
  starting: 'bg-status-working animate-pulse',
  'waking…': 'bg-status-working animate-pulse',
  unreachable: 'bg-status-failed',
  unavailable: 'bg-status-failed',
  'last message failed': 'bg-status-failed',
  ready: 'bg-term-green',
  'connecting…': 'bg-status-idle animate-pulse',
  'reconnecting…': 'bg-status-idle animate-pulse',
  idle: 'bg-status-idle',
  asleep: 'bg-status-idle',
  paused: 'bg-status-idle'
};

/** The status marker's colour class. The word always shows beside it, so colour is never alone. */
export function barStatusDot(status: BarStatus): string {
  return DOT[status];
}
