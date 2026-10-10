import type { Session } from './api';

/** Current attention wins over conversation metadata, including during a late response. */
export function zoomSession(
  id: string,
  conversation: Session | null,
  projected: Session[],
  loaded: boolean,
  reconciled: Session | null
): Session | null {
  return (
    projected.find((session) => session.id === id) ??
    (loaded
      ? reconciled?.id === id
        ? reconciled
        : null
      : conversation?.id === id
        ? conversation
        : null)
  );
}
