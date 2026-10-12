import { ApiError, REFUSAL_CODES } from '@/api/errors';
import type {
  MainProvider,
  MainProviderInfo,
  MainProviderRefusalCode,
  MainProviderSelection,
  MainThreadInfo
} from './types';

/** The provider select's options, in the order it lists them. */
export const MAIN_PROVIDERS: { value: MainProvider; label: string }[] = [
  { value: 'claude', label: 'Claude' },
  { value: 'codex', label: 'Codex' }
];

/** The most backend or error text the provider row shows; a longer message is cut here. */
export const NOTICE_MAX = 300;

/** Shown while a read of the provider state has not succeeded since a selection. */
export const UNCONFIRMED_NOTICE =
  'Could not confirm provider state. Waiting for a successful read before allowing another selection or main message.';

/** Delivery states that keep the main provider in use: a switch waits until none of them remain. */
const BUSY_DELIVERY_STATES = ['recorded', 'sending', 'delivered', 'uncertain'];

/** The owner's controls for the main provider, as the header and the page share them. */
export interface MainProviderControl {
  /** A selection's write or confirming read is running. */
  selecting: boolean;
  /** The last read of the provider state failed, so it is not known to be current. */
  unconfirmed: boolean;
  /** The select and Restart are held: see `providerSwitchBlocked`. */
  switchBlocked: boolean;
  /** Retry is held: see `providerRetryBlocked`. It is held by the same rules, minus a pending switch. */
  retryBlocked: boolean;
  /** What the last selection said, bounded; null when there is nothing to say. */
  notice: string | null;
  /** Retry repeats the last failed write, or the desired provider when the start failed or is pending. */
  retryOffered: boolean;
  /** Restart sends `restart_failed: true`; offered only for a recoverable failed start. */
  restartOffered: boolean;
  select: (provider: MainProvider) => void;
  retry: () => void;
  restart: () => void;
}

export function providerLabel(provider: MainProvider): string {
  return MAIN_PROVIDERS.find((option) => option.value === provider)?.label ?? provider;
}

/** Cuts text to the notice bound. The backend's own error strings are already short. */
export function boundedText(text: string): string {
  return text.trim().slice(0, NOTICE_MAX);
}

/**
 * What holds every provider change. `busy` is what the thread does not know itself: offline, a
 * selection in flight, a send or stop running, or the thread not yet read.
 */
function providerHeld(info: MainThreadInfo | null | undefined, busy: boolean): boolean {
  return (
    busy ||
    !info?.provider ||
    info.native?.turn_in_flight === true ||
    (info.native?.deliveries ?? []).some((delivery) =>
      BUSY_DELIVERY_STATES.includes(delivery.state)
    )
  );
}

/** Whether the owner may pick a provider or restart now. A pending switch holds both. */
export function providerSwitchBlocked(
  info: MainThreadInfo | null | undefined,
  busy: boolean
): boolean {
  return providerHeld(info, busy) || info?.provider?.status === 'pending';
}

/**
 * Whether the owner may retry now. Retry repeats the write a pending switch already holds, so a
 * pending switch does not hold it; every other hold does.
 */
export function providerRetryBlocked(
  info: MainThreadInfo | null | undefined,
  busy: boolean
): boolean {
  return providerHeld(info, busy);
}

/** Ready means the confirmed provider is the one the owner selected. */
export function providerReady(provider: MainProviderInfo | null | undefined): boolean {
  return provider?.status === 'ready' && provider.effective_provider === provider.desired_provider;
}

/** Choosing the provider that is already ready changes nothing, so it sends nothing. */
export function selectionIsNoop(
  provider: MainProvider,
  current: MainProviderInfo | null | undefined
): boolean {
  return providerReady(current) && current?.desired_provider === provider;
}

/**
 * Why the main composer takes no message, or null when it can. A `not_started` main thread takes
 * its first message: the provider starts with it. Any other state that is not ready locks the box.
 */
export function mainInputLock(
  info: MainThreadInfo | null | undefined,
  state: Pick<MainProviderControl, 'selecting' | 'unconfirmed'>
): string | null {
  // Before the thread is read the composer is already in its loading state.
  if (!info) return null;
  if (state.selecting) return 'Main provider is changing. Messages wait for it to finish.';
  if (state.unconfirmed)
    return 'Main provider state is unconfirmed. Messages wait for a successful read.';
  const provider = info.provider;
  if (!provider) return 'Main provider state is unknown.';
  switch (provider.status) {
    case 'not_started':
      return null;
    case 'ready':
      return providerReady(provider) ? null : 'Main provider is not switched yet.';
    case 'pending':
      return 'Main provider is preparing.';
    case 'failed':
      return 'Main provider failed to start. Retry or restart it above.';
    default:
      return 'Main provider state is unknown.';
  }
}

/**
 * Whether the provider row shows without opening the thread's details: the provider is failed,
 * switching, unknown or unconfirmed, the thread is offline, or the last selection left a notice.
 * A provider that is ready, or not started yet, stays in the details.
 */
export function providerNeedsAttention(
  info: MainThreadInfo | null | undefined,
  state: { offline: boolean; selecting: boolean; unconfirmed: boolean; notice: string | null }
): boolean {
  if (state.offline || state.selecting || state.unconfirmed || state.notice) return true;
  const provider = info?.provider;
  if (!provider) return true;
  return provider.status !== 'not_started' && !providerReady(provider);
}

/** The state words for the provider row: the request in progress and the read state come first. */
export function providerStateWords(
  provider: MainProviderInfo | null | undefined,
  state: { offline: boolean; selecting: boolean; unconfirmed: boolean }
): string {
  if (state.offline) return 'offline, selection unavailable';
  if (state.selecting) return 'selecting provider…';
  if (state.unconfirmed) return 'provider state unconfirmed';
  if (!provider) return 'state unknown';
  switch (provider.status) {
    case 'not_started':
      return 'not started, starts with your next message';
    case 'pending':
      return 'preparing';
    case 'failed':
      return 'failed';
    case 'ready':
      return providerReady(provider) ? 'ready' : 'waiting to switch';
    default:
      return 'state unknown';
  }
}

/**
 * Whether Retry is offered. A failed start always offers it. A pending switch does too: repeating
 * its write is how a request that died is recovered. A failed write offers it until a read shows
 * that write took effect.
 */
export function retryOffered(
  provider: MainProviderInfo | null | undefined,
  failed: MainProviderSelection | null
): boolean {
  if (provider?.status === 'failed' || provider?.status === 'pending') return true;
  if (!failed) return false;
  return !(providerReady(provider) && provider?.desired_provider === failed.provider);
}

/** The sentence every refusal notice ends with: the state is re-read and nothing is retried. */
const REFRESHED = 'State is refreshed; no automatic retry.';

/**
 * The notice for each refusal code. Written here, not taken from the server's prose, so the owner
 * reads the same words whatever the backend wording. `agent_unavailable` names the provider asked for.
 */
const REFUSAL_NOTICE: Record<MainProviderRefusalCode, (requested: MainProvider) => string> = {
  busy: () => 'Provider change refused: the main has open work or changed while the request ran.',
  pending: () =>
    'Provider change refused: a switch is already pending and must settle or be recoverable first.',
  unconfirmed: () => 'Provider change refused: kagent could not confirm the main is idle.',
  unresolved: () => 'Provider change refused: a native create for the main is still unresolved.',
  agent_unavailable: (requested) =>
    `${providerLabel(requested)} isn't installed here. Nothing changed.`
};

/** The notice for a chat message the backend refused because of the main's provider state. */
const SEND_REFUSAL_NOTICE: Record<MainProviderRefusalCode, string> = {
  busy: 'Message not sent: the main has open work or changed while the request ran.',
  pending: 'Message not sent: a provider switch is pending.',
  unconfirmed: 'Message not sent: kagent could not confirm the main is idle.',
  unresolved: 'Message not sent: a native create for the main is unresolved.',
  agent_unavailable: "Message not sent: the other provider isn't installed here. Nothing changed."
};

/** The notice for a refused chat message, from its code. The caller adds the box note. */
export function sendRefusalNotice(code: MainProviderRefusalCode): string {
  return SEND_REFUSAL_NOTICE[code];
}

/**
 * The refusal code a failed request carries, or null. Only a code the contract names counts; any
 * other value, or none, leaves the notice to the server's detail text.
 */
export function refusalCode(error: unknown): MainProviderRefusalCode | null {
  const code = typeof error === 'object' && error !== null && 'code' in error ? error.code : null;
  return REFUSAL_CODES.find((known) => known === code) ?? null;
}

/**
 * The words for a selection the backend did not confirm. A known refusal code picks the notice;
 * otherwise the detail is shown. A 409 is a block, not a lost write.
 */
export function selectionNotice(error: unknown, requested: MainProvider): string {
  const code = refusalCode(error);
  if (code) return `${REFUSAL_NOTICE[code](requested)} ${REFRESHED}`;
  // The detail may end in a full stop of its own; the sentence after it adds one.
  const detail = boundedText(
    error instanceof Error && error.message ? error.message : 'request failed'
  ).replace(/\.+$/, '');
  if (error instanceof ApiError && error.status === 409)
    return `Provider change blocked (409): ${detail}. ${REFRESHED}`;
  return `Provider selection was not confirmed: ${detail}. ${REFRESHED}`;
}

/**
 * A token per request. A result is applied only when no newer request has started, so a slow
 * earlier read cannot overwrite a later selection.
 */
export function createRequestToken() {
  let latest = 0;
  return {
    next: () => ++latest,
    isLatest: (token: number) => token === latest
  };
}
