/**
 * The one status a session's row shows. The thread (delivery ledger and reachability) and the live
 * view (its channel and the runner's turn) each know part of it; `barStatus` puts them in one
 * order, so the row never says "ready" beside "asleep".
 */
import type { ThreadStatus } from './delivery';
import type { Tone } from './taskState';

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

/**
 * The status's place in the status language, so its icon is the sidebar's: needs you and an
 * unconfirmed message are the owner's turn; a turn running, starting or waking is working; the
 * backend unreachable, the channel unavailable or the last message failed is a failure; everything
 * else is at rest, including still trying to connect.
 */
const TONE: Record<BarStatus, Tone> = {
  'needs you': 'attention',
  'last message unconfirmed': 'attention',
  working: 'active',
  starting: 'active',
  'waking…': 'active',
  unreachable: 'bad',
  unavailable: 'bad',
  'last message failed': 'bad',
  ready: 'muted',
  'connecting…': 'muted',
  'reconnecting…': 'muted',
  idle: 'muted',
  asleep: 'muted',
  paused: 'muted'
};

export function barStatusTone(status: BarStatus): Tone {
  return TONE[status];
}
