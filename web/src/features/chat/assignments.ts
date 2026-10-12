import type { NativeDelivery } from '@/lib/contract';

/**
 * The messages Mainloop sent on the owner's behalf. A delivery's `source` says who wrote it; the
 * owner's own messages are `user`, the contract's default. Anything else is an assignment, such as
 * a delegated brief, and is shown as one rather than as a message from "You".
 */
export function assignmentIds(deliveries: NativeDelivery[] | undefined): ReadonlySet<string> {
  return new Set(
    (deliveries ?? [])
      .filter((delivery) => (delivery.source ?? 'user') !== 'user')
      .map((delivery) => delivery.message_id)
  );
}
