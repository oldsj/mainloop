/** What the owner sees about a message that was not delivered, from the ledger's delivery rows. */
import type { NativeDelivery } from './api';

export interface DeliveryNotice {
  state: 'failed' | 'uncertain';
  /** Short headline: whether the agent ever saw the message. */
  label: string;
  /** The recorded reason (error class and message), if any. */
  reason: string | null;
  /** A new message with the same text may be sent. Never the same message id. */
  retryable: boolean;
}

/** Thread status for the header and identity strip, most urgent first. */
export type ThreadStatus = 'unreachable' | 'working' | 'failed' | 'unconfirmed' | 'idle' | 'ready';

const OPEN_STATES = ['recorded', 'sending', 'delivered'];

/**
 * Notices keyed by message id, for every failed or uncertain delivery.
 *
 * Only the newest delivery of a user message can be retried, and only while nothing is in
 * flight: an older failure has been superseded, and a retry is a new message that the backend
 * would refuse (409) during a turn.
 */
export function deliveryNotices(deliveries: NativeDelivery[]): Map<string, DeliveryNotice> {
  const notices = new Map<string, DeliveryNotice>();
  const last = deliveries.at(-1);
  const inFlight = deliveries.some((d) => OPEN_STATES.includes(d.state));
  for (const d of deliveries) {
    if (d.state !== 'failed' && d.state !== 'uncertain') continue;
    const neverSent = d.state === 'failed' && (d.detail ?? '').startsWith('not sent');
    notices.set(d.message_id, {
      state: d.state,
      label: d.state === 'uncertain' ? 'Delivery unconfirmed' : neverSent ? 'Not sent' : 'Failed',
      reason: d.detail,
      retryable:
        d.state === 'failed' &&
        (d.source ?? 'user') === 'user' &&
        last?.message_id === d.message_id &&
        !inFlight
    });
  }
  return notices;
}

/** The thread's status. A failed or unconfirmed last delivery is never shown as ready or working. */
export function threadStatus(opts: {
  offline: boolean;
  deliveries: NativeDelivery[];
  sessionState?: string | null;
}): ThreadStatus {
  if (opts.offline) return 'unreachable';
  if (opts.deliveries.some((d) => OPEN_STATES.includes(d.state))) return 'working';
  const last = opts.deliveries.at(-1);
  if (last?.state === 'failed') return 'failed';
  if (last?.state === 'uncertain') return 'unconfirmed';
  return opts.sessionState === 'suspended' ? 'idle' : 'ready';
}
