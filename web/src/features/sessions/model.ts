/**
 * Pure rules for the sessions UI: what the list shows, how an agent kind resolves, how a failed
 * read is classified, and how action results read. Kept free of React so each rule has a unit
 * test that does not need a router or a query client.
 */
import { ApiError, type components } from '@/api';
import { sessionStatusTone } from '@/components/shared/status-tokens';
import type { NativeDelivery, ProviderProfile, Session } from '@/lib/contract';
import { threadStatus } from '@/lib/delivery';
import { isCurrent, isFinished } from '@/lib/sessionStatus';
import type { Tone } from '@/lib/taskState';

export type WorkspaceLifecycle = components['schemas']['WorkspaceLifecycle'];
type CancelResult = components['schemas']['SessionCancelResponse'];
type StopTurnResult = components['schemas']['SessionStopTurnResponse'];

export type NativeProvider = ProviderProfile['native_provider'];

/**
 * The fields the list row and its status words read. A project's session rows are checked against
 * this same set, so a row that lacks one of them fails tsc rather than rendering blank.
 */
export type SessionRowFields = Pick<
  Session,
  | 'id'
  | 'title'
  | 'status'
  | 'needs_owner'
  | 'created_at'
  | 'agent_kind'
  | 'topic'
  | 'error'
  | 'repo_url'
  | 'parent_session_id'
>;

/** The provider fields a session row reads to name its agent. */
export type ProviderFields = Pick<ProviderProfile, 'id' | 'native_provider' | 'aliases'>;

/** The workspace fields the badges and the session row read. */
export type WorkspaceBadgeFields = Pick<
  WorkspaceLifecycle,
  | 'workspace_id'
  | 'session_id'
  | 'observed_state'
  | 'detail'
  | 'publication_mode'
  | 'publication_reason'
> & { manifest: Pick<WorkspaceLifecycle['manifest'], 'agent_kind'> };

/** How often the identity strip re-reads native state, and only while a turn is in flight. */
export const NATIVE_POLL_MS = 3_000;

/**
 * The list rows for the two filters. Current sessions always show; finished sessions show when
 * "Show finished" is on, and idle ones (neither current nor finished) when "Show idle" is on.
 */
export function visibleSessions(
  sessions: Session[],
  filters: { showFinished: boolean; showIdle: boolean }
): Session[] {
  return sessions.filter(
    (session) =>
      isCurrent(session) ||
      (filters.showFinished && isFinished(session)) ||
      (filters.showIdle && !isFinished(session))
  );
}

/** Badge tone. A session waiting on the owner reads as attention, whatever its status. */
export function sessionTone(session: SessionRowFields): Tone {
  if (session.needs_owner && !isFinished(session)) return 'attention';
  return sessionStatusTone(session.status);
}

/** Counts for the header and the filter buttons. Each session falls into exactly one bucket. */
export function sessionCounts(sessions: Session[]): {
  current: number;
  finished: number;
  idle: number;
} {
  let current = 0;
  let finished = 0;
  let idle = 0;
  for (const session of sessions) {
    if (isFinished(session)) finished += 1;
    else if (isCurrent(session)) current += 1;
    else idle += 1;
  }
  return { current, finished, idle };
}

/**
 * The native provider behind a session's agent kind. `agent_kind` is a provider profile ID or
 * alias, so it resolves through `/providers`. The reserved IDs `claude` and `codex` resolve even
 * before the profile list has loaded. Anything else is unknown, and the UI shows "agent".
 */
export function nativeProviderFor(
  kind: string | null | undefined,
  providers: ProviderFields[] = []
): NativeProvider | null {
  if (!kind) return null;
  const profile = providers.find((item) => item.id === kind || item.aliases.includes(kind));
  if (profile) return profile.native_provider;
  if (kind === 'claude' || kind === 'codex') return kind;
  return null;
}

export type LoadFailure = 'not-found' | 'server' | 'unreachable' | 'other';

/**
 * Sorts a failed read into the states the page shows. Only an HTTP response counts as the backend
 * answering: a 404 is "not found", a 5xx is a server error, and a fetch that got no response or a
 * read that timed out is "unreachable", which is retried rather than reported as missing. Anything
 * else (a malformed body, a client bug) is "other", an error with its own message.
 */
export function loadFailure(error: unknown): LoadFailure {
  if (error instanceof ApiError) {
    if (error.status === 404) return 'not-found';
    if (error.status >= 500) return 'server';
    return 'other';
  }
  // A failed fetch rejects with a TypeError; the read timeout is a DOMException named TimeoutError.
  if (error instanceof TypeError) return 'unreachable';
  if (error instanceof DOMException && error.name === 'TimeoutError') return 'unreachable';
  return 'other';
}

export interface Notice {
  tone: 'ok' | 'warn';
  text: string;
}

/** What a cancel result means for the owner. An unknown agent state is a warning, never silent success. */
export function cancelNotice(result: CancelResult): Notice {
  if (result.agent === 'unknown') {
    return {
      tone: 'warn',
      text: "Cancelled, but Mainloop couldn't confirm the agent stopped; it may still be running."
    };
  }
  if (result.status === 'cancelled') return { tone: 'ok', text: 'Session cancelled.' };
  return { tone: 'ok', text: `Session had already ${result.status}.` };
}

/** A failed action, as a notice. A 409 or 502 carries the backend's own wording; no response means the backend is unreachable. */
export function actionFailure(error: unknown): Notice {
  if (error instanceof ApiError) return { tone: 'warn', text: error.message };
  return { tone: 'warn', text: "Couldn't reach Mainloop. Try again when it's back." };
}

const STOP_NOTICES: Record<StopTurnResult['status'], string> = {
  stopped: 'Turn stopped. The session is still open.',
  finished: 'The turn had already finished.',
  no_open_turn: 'No turn was running.'
};

export function stopNotice(result: StopTurnResult): Notice {
  return { tone: 'ok', text: STOP_NOTICES[result.status] };
}

/**
 * Why the session takes no more messages, or null when it does. The backend refuses sends to an
 * archived session and to a failed or cancelled one (docs/specs/sessions.md). A completed session
 * still takes messages.
 */
export function chatClosedReason(session: Pick<Session, 'status' | 'archived_at'>): string | null {
  if (session.archived_at) return 'This session is archived, so it takes no new messages.';
  if (session.status === 'failed') return 'This session failed, so it takes no new messages.';
  if (session.status === 'cancelled')
    return 'This session was cancelled, so it takes no new messages.';
  return null;
}

/**
 * The one-word state in the identity strip, worded as the frozen app words it. Offline shows
 * `unreachable`. A failed or unconfirmed last delivery is never shown as live, whatever the agent
 * session reports. Otherwise the agent's own state shows: `live` for ready, and the raw state for
 * anything else. With no state from the agent, the word is `unknown`.
 */
export function liveWord(opts: {
  native: { deliveries: NativeDelivery[]; session_state: string | null } | undefined;
  offline: boolean;
}): string {
  if (opts.offline) return 'unreachable';
  const status = threadStatus({ offline: false, deliveries: opts.native?.deliveries ?? [] });
  if (status === 'failed') return 'last message failed';
  if (status === 'unconfirmed') return 'last message unconfirmed';
  const state = opts.native?.session_state;
  if (!state) return 'unknown';
  return state === 'ready' ? 'live' : state;
}

/** Turn-in-flight drives polling; a settled session is read once and left alone. */
export function nativePollInterval(
  native: { turn_in_flight: boolean } | undefined
): number | false {
  return native?.turn_in_flight ? NATIVE_POLL_MS : false;
}

/** Relative age for list rows, in the same buckets the frozen app used. */
export function relativeTime(iso: string, now: number = Date.now()): string {
  const minutes = Math.floor((now - Date.parse(iso)) / 60_000);
  const hours = Math.floor(minutes / 60);
  const days = Math.floor(hours / 24);
  if (days > 0) return `${days}d ago`;
  if (hours > 0) return `${hours}h ago`;
  if (minutes > 0) return `${minutes}m ago`;
  return 'just now';
}

export function workspaceFor<T extends { session_id: string }>(
  workspaces: T[],
  sessionId: string
): T | undefined {
  return workspaces.find((workspace) => workspace.session_id === sessionId);
}

/** Tone for the workspace's observed state: running is active, a failure is bad, anything in motion needs attention. */
export function workspaceTone(state: WorkspaceLifecycle['observed_state']): Tone {
  if (state === 'running') return 'active';
  if (state === 'failed') return 'bad';
  if (state === 'suspended') return 'muted';
  return 'attention';
}

export function publicationText(
  workspace: Pick<WorkspaceLifecycle, 'publication_mode' | 'publication_reason'>
): string {
  if (workspace.publication_mode === 'branch') return 'Branch publication';
  const reason = workspace.publication_reason?.replaceAll('_', ' ') ?? 'no grant';
  return `Read only · ${reason}`;
}
