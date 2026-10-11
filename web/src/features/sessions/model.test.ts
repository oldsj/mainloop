import { describe, expect, it } from 'vitest';
import { ApiError } from '@/api';
import { workspaceFixture } from '@/test/fixtures';
import { CLAUDE_PROFILE, CODEX_PROFILE, profile } from '@/test/fixtures/tasks';
import { delivery, native, session } from './test/fixtures';
import {
  actionFailure,
  cancelNotice,
  liveWord,
  loadFailure,
  NATIVE_POLL_MS,
  nativePollInterval,
  nativeProviderFor,
  publicationText,
  relativeTime,
  sessionCounts,
  sessionTone,
  stopNotice,
  visibleSessions,
  workspaceTone
} from './model';

const ids = (sessions: { id: string }[]) => sessions.map((item) => item.id);

const mixed = [
  session({ id: 'a', status: 'active' }),
  session({ id: 'p', status: 'pending' }),
  session({ id: 'n', status: 'waiting_on_user', needs_owner: true }),
  session({ id: 'i', status: 'idle' }),
  session({ id: 'c', status: 'completed' }),
  session({ id: 'f', status: 'failed' }),
  session({ id: 'x', status: 'cancelled' }),
  session({ id: 'u', status: 'superseded' })
];

describe('visibleSessions and sessionCounts', () => {
  it('shows only current sessions by default', () => {
    expect(ids(visibleSessions(mixed, { showFinished: false, showIdle: false }))).toEqual([
      'a',
      'p',
      'n'
    ]);
  });

  it('adds finished sessions when "show finished" is on', () => {
    expect(ids(visibleSessions(mixed, { showFinished: true, showIdle: false }))).toEqual([
      'a',
      'p',
      'n',
      'c',
      'f',
      'x',
      'u'
    ]);
  });

  it('adds idle sessions when "show idle" is on', () => {
    expect(ids(visibleSessions(mixed, { showFinished: false, showIdle: true }))).toEqual([
      'a',
      'p',
      'n',
      'i'
    ]);
  });

  it('counts each session in exactly one bucket', () => {
    expect(sessionCounts(mixed)).toEqual({ current: 3, finished: 4, idle: 1 });
  });
});

describe('sessionTone', () => {
  it('reads a live session that needs the owner as attention', () => {
    expect(sessionTone(session({ status: 'waiting_on_user', needs_owner: true }))).toBe(
      'attention'
    );
  });

  it('does not treat a finished session as needing attention', () => {
    expect(sessionTone(session({ status: 'failed', needs_owner: true }))).not.toBe('attention');
  });
});

describe('nativeProviderFor', () => {
  it('resolves a profile ID to its native provider', () => {
    expect(nativeProviderFor('codex-default', [CODEX_PROFILE])).toBe('codex');
  });

  it('resolves an alias to its native provider', () => {
    const aliased = profile({
      id: 'team-claude',
      native_provider: 'claude',
      aliases: ['claude-main']
    });
    expect(nativeProviderFor('claude-main', [aliased])).toBe('claude');
  });

  it('resolves the reserved kinds before the profile list loads', () => {
    expect(nativeProviderFor('codex')).toBe('codex');
    expect(nativeProviderFor('claude', [])).toBe('claude');
  });

  it('returns null for an unknown kind or none', () => {
    expect(nativeProviderFor('mystery', [CLAUDE_PROFILE])).toBeNull();
    expect(nativeProviderFor(null, [CLAUDE_PROFILE])).toBeNull();
  });
});

describe('loadFailure', () => {
  it('treats anything without an HTTP response as unreachable', () => {
    expect(loadFailure(new TypeError('Failed to fetch'))).toBe('unreachable');
  });

  it('separates not-found, server and other HTTP failures', () => {
    expect(loadFailure(new ApiError('Session not found', 404))).toBe('not-found');
    expect(loadFailure(new ApiError('boom', 500))).toBe('server');
    expect(loadFailure(new ApiError('boom', 503))).toBe('server');
    expect(loadFailure(new ApiError('forbidden', 403))).toBe('other');
  });
});

describe('action notices', () => {
  it('reports a confirmed cancel', () => {
    expect(cancelNotice({ status: 'cancelled', agent: 'stopped' })).toEqual({
      tone: 'ok',
      text: 'Session cancelled.'
    });
  });

  it('warns when the agent state after a cancel is unknown', () => {
    const notice = cancelNotice({ status: 'cancelled', agent: 'unknown' });
    expect(notice.tone).toBe('warn');
    expect(notice.text).toContain("couldn't confirm the agent stopped");
  });

  it('says so when the session had already finished', () => {
    expect(cancelNotice({ status: 'completed', agent: 'not_running' }).text).toBe(
      'Session had already completed.'
    );
  });

  it('describes each stop-turn outcome', () => {
    expect(stopNotice({ status: 'stopped' }).text).toBe('Turn stopped. The session is still open.');
    expect(stopNotice({ status: 'finished' }).text).toBe('The turn had already finished.');
    expect(stopNotice({ status: 'no_open_turn' }).text).toBe('No turn was running.');
  });

  it('shows the backend message for a failed action, and a reachability note otherwise', () => {
    expect(actionFailure(new ApiError('A task is still running.', 409))).toEqual({
      tone: 'warn',
      text: 'A task is still running.'
    });
    expect(actionFailure(new TypeError('Failed to fetch')).text).toBe(
      "Couldn't reach Mainloop. Try again when it's back."
    );
  });
});

describe('liveWord', () => {
  it('shows unreachable while offline, whatever the agent reports', () => {
    expect(liveWord({ native: native(), offline: true })).toBe('unreachable');
  });

  it('never shows ready after a failed last delivery', () => {
    const failed = native({ deliveries: [delivery({ state: 'failed', detail: 'refused' })] });
    expect(liveWord({ native: failed, offline: false })).toBe('last message failed');
  });

  it('never shows ready after an unconfirmed last delivery', () => {
    const uncertain = native({ deliveries: [delivery({ state: 'uncertain' })] });
    expect(liveWord({ native: uncertain, offline: false })).toBe('last message unconfirmed');
  });

  it('shows the agent state while a delivery is in flight, as the frozen app does', () => {
    const sending = native({
      deliveries: [delivery({ state: 'sending' })],
      session_state: 'busy'
    });
    expect(liveWord({ native: sending, offline: false })).toBe('busy');
  });

  it('shows unknown before any native read, or with no agent state', () => {
    expect(liveWord({ native: undefined, offline: false })).toBe('unknown');
    expect(liveWord({ native: native({ session_state: null }), offline: false })).toBe('unknown');
  });

  it('shows live for ready, and any other agent state as it is', () => {
    expect(liveWord({ native: native({ session_state: 'ready' }), offline: false })).toBe('live');
    expect(liveWord({ native: native({ session_state: 'suspended' }), offline: false })).toBe(
      'suspended'
    );
    expect(liveWord({ native: native({ session_state: 'busy' }), offline: false })).toBe('busy');
  });
});

describe('nativePollInterval', () => {
  it('polls every 3 seconds only while a turn is in flight', () => {
    expect(nativePollInterval({ turn_in_flight: true })).toBe(NATIVE_POLL_MS);
    expect(NATIVE_POLL_MS).toBe(3_000);
    expect(nativePollInterval({ turn_in_flight: false })).toBe(false);
    expect(nativePollInterval(undefined)).toBe(false);
  });
});

describe('relativeTime', () => {
  const now = Date.parse('2026-01-01T12:00:00Z');

  it('uses the same age buckets as the list rows', () => {
    expect(relativeTime('2026-01-01T11:30:00Z', now)).toBe('30m ago');
    expect(relativeTime('2026-01-01T09:00:00Z', now)).toBe('3h ago');
    expect(relativeTime('2025-12-30T12:00:00Z', now)).toBe('2d ago');
    expect(relativeTime('2026-01-01T11:59:50Z', now)).toBe('just now');
  });
});

describe('workspace helpers', () => {
  it('describes publication as branch, or read only with its reason', () => {
    expect(publicationText(workspaceFixture({ publication_mode: 'branch' }))).toBe(
      'Branch publication'
    );
    expect(
      publicationText(
        workspaceFixture({ publication_mode: 'read_only', publication_reason: 'protected_branch' })
      )
    ).toBe('Read only · protected branch');
    expect(
      publicationText(workspaceFixture({ publication_mode: 'read_only', publication_reason: null }))
    ).toBe('Read only · no grant');
  });

  it('tones the observed state', () => {
    expect(workspaceTone('running')).toBe('active');
    expect(workspaceTone('failed')).toBe('bad');
    expect(workspaceTone('suspended')).toBe('muted');
    expect(workspaceTone('resuming')).toBe('attention');
  });
});
