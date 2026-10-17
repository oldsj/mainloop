import { describe, expect, it } from 'vitest';
import { ApiError, type RefusalCode } from '@/api/errors';
import { deliveryFixture, mainThreadFixture, nativeFixture, providerFixture } from './fixtures';
import {
  boundedText,
  createRequestToken,
  mainInputLock,
  NOTICE_MAX,
  providerReady,
  providerStateWords,
  providerRetryBlocked,
  providerSwitchBlocked,
  refusalCode,
  retryOffered,
  selectionIsNoop,
  selectionNotice
} from './mainProvider';
import type { MainProviderSelection, MainThreadInfo } from './types';

const idle = { selecting: false, unconfirmed: false };
const idleWords = { offline: false, selecting: false, unconfirmed: false };

describe('providerSwitchBlocked', () => {
  it('is open for a ready provider with nothing in flight', () => {
    expect(providerSwitchBlocked(mainThreadFixture(), false)).toBe(false);
  });

  it('is held while the page is busy: offline, sending, stopping, a selection, or not yet read', () => {
    expect(providerSwitchBlocked(mainThreadFixture(), true)).toBe(true);
  });

  it('is held before the thread or its provider is known', () => {
    expect(providerSwitchBlocked(null, false)).toBe(true);
    expect(providerSwitchBlocked(undefined, false)).toBe(true);
    expect(providerSwitchBlocked(mainThreadFixture({ provider: null }), false)).toBe(true);
  });

  it('is held while the provider is preparing', () => {
    expect(
      providerSwitchBlocked(
        mainThreadFixture({ provider: providerFixture({ status: 'pending' }) }),
        false
      )
    ).toBe(true);
  });

  it('is held while a turn is open', () => {
    expect(
      providerSwitchBlocked(
        mainThreadFixture({ native: nativeFixture({ turn_in_flight: true }) }),
        false
      )
    ).toBe(true);
  });

  it.each(['recorded', 'sending', 'delivered', 'uncertain', 'queued'])(
    'is held while a delivery is %s',
    (state) => {
      expect(
        providerSwitchBlocked(
          mainThreadFixture({
            native: nativeFixture({ deliveries: [deliveryFixture({ state })] })
          }),
          false
        )
      ).toBe(true);
    }
  );

  it.each(['completed', 'failed', 'cancelled'])(
    'stays open when the last delivery is %s',
    (state) => {
      expect(
        providerSwitchBlocked(
          mainThreadFixture({
            native: nativeFixture({ deliveries: [deliveryFixture({ state })] })
          }),
          false
        )
      ).toBe(false);
    }
  );

  it.each(['ready', 'not_started', 'failed'] as const)(
    'stays open for a %s provider, so a failed start can be retried',
    (status) => {
      expect(
        providerSwitchBlocked(mainThreadFixture({ provider: providerFixture({ status }) }), false)
      ).toBe(false);
    }
  );
});

describe('providerRetryBlocked', () => {
  const pending = mainThreadFixture({ provider: providerFixture({ status: 'pending' }) });

  it('is open for a ready provider with nothing in flight', () => {
    expect(providerRetryBlocked(mainThreadFixture(), false)).toBe(false);
  });

  it('is open while a switch is pending, so Retry can repeat it', () => {
    expect(providerRetryBlocked(pending, false)).toBe(false);
  });

  it('is held while the page is busy, even when a switch is pending', () => {
    expect(providerRetryBlocked(pending, true)).toBe(true);
  });

  it('is held while a turn is open, even when a switch is pending', () => {
    expect(
      providerRetryBlocked(
        mainThreadFixture({
          provider: providerFixture({ status: 'pending' }),
          native: nativeFixture({ turn_in_flight: true })
        }),
        false
      )
    ).toBe(true);
  });

  it.each(['recorded', 'sending', 'delivered', 'uncertain'])(
    'is held while a delivery is %s',
    (state) => {
      expect(
        providerRetryBlocked(
          mainThreadFixture({
            native: nativeFixture({ deliveries: [deliveryFixture({ state })] })
          }),
          false
        )
      ).toBe(true);
    }
  );

  it('is held before the thread or its provider is known', () => {
    expect(providerRetryBlocked(null, false)).toBe(true);
    expect(providerRetryBlocked(mainThreadFixture({ provider: null }), false)).toBe(true);
  });
});

describe('providerReady', () => {
  it.each([
    ['ready and matching', providerFixture(), true],
    [
      'ready but still on the other provider',
      providerFixture({ desired_provider: 'codex' }),
      false
    ],
    ['not started', providerFixture({ status: 'not_started', effective_provider: null }), false],
    ['pending', providerFixture({ status: 'pending' }), false],
    ['failed', providerFixture({ status: 'failed' }), false],
    ['unknown', null, false]
  ])('%s is %s', (_label, provider, expected) => {
    expect(providerReady(provider)).toBe(expected);
  });
});

describe('selectionIsNoop', () => {
  it('does nothing when the ready provider is chosen again', () => {
    expect(selectionIsNoop('claude', providerFixture())).toBe(true);
  });

  it('sends when a different provider is chosen', () => {
    expect(selectionIsNoop('codex', providerFixture())).toBe(false);
  });

  it('sends when the chosen provider is not ready, so a failed start can be retried', () => {
    expect(selectionIsNoop('claude', providerFixture({ status: 'failed' }))).toBe(false);
  });

  it('sends when there is no provider state yet', () => {
    expect(selectionIsNoop('claude', null)).toBe(false);
  });
});

describe('mainInputLock', () => {
  const info = (provider: ReturnType<typeof providerFixture> | null): MainThreadInfo =>
    mainThreadFixture({ provider });

  it.each([
    ['not_started', providerFixture({ status: 'not_started', effective_provider: null }), null],
    ['ready and matching', providerFixture(), null],
    [
      'ready but not switched yet',
      providerFixture({ desired_provider: 'codex' }),
      'Main provider is not switched yet.'
    ],
    ['pending', providerFixture({ status: 'pending' }), 'Main provider is preparing.'],
    [
      'failed',
      providerFixture({ status: 'failed' }),
      'Main provider failed to start. Retry or restart it above.'
    ],
    ['no provider state', null, 'Main provider state is unknown.']
  ])('%s', (_label, provider, expected) => {
    expect(mainInputLock(info(provider), idle)).toBe(expected);
  });

  it('is locked while a selection is in flight, whatever the provider state', () => {
    expect(mainInputLock(info(providerFixture()), { selecting: true, unconfirmed: false })).toBe(
      'Main provider is changing. Messages wait for it to finish.'
    );
  });

  it('is locked until a read confirms the provider state', () => {
    expect(mainInputLock(info(providerFixture()), { selecting: false, unconfirmed: true })).toBe(
      'Main provider state is unconfirmed. Messages wait for a successful read.'
    );
  });

  it('leaves the loading state to the composer when the thread is not read yet', () => {
    expect(mainInputLock(undefined, { selecting: true, unconfirmed: true })).toBeNull();
  });

  it('treats an unrecognised status as unknown', () => {
    const odd = providerFixture({ status: 'mystery' as never });
    expect(mainInputLock(info(odd), idle)).toBe('Main provider state is unknown.');
  });
});

describe('providerStateWords', () => {
  it.each([
    [
      'offline',
      null,
      { offline: true, selecting: false, unconfirmed: false },
      'offline, selection unavailable'
    ],
    [
      'selecting',
      providerFixture(),
      { offline: false, selecting: true, unconfirmed: false },
      'selecting provider…'
    ],
    [
      'unconfirmed',
      providerFixture(),
      { offline: false, selecting: false, unconfirmed: true },
      'provider state unconfirmed'
    ],
    ['no state', null, idleWords, 'state unknown'],
    [
      'not started',
      providerFixture({ status: 'not_started', effective_provider: null }),
      idleWords,
      'not started, starts with your next message'
    ],
    ['pending', providerFixture({ status: 'pending' }), idleWords, 'preparing'],
    ['failed', providerFixture({ status: 'failed' }), idleWords, 'failed'],
    ['ready', providerFixture(), idleWords, 'ready'],
    [
      'ready but not switched yet',
      providerFixture({ desired_provider: 'codex' }),
      idleWords,
      'waiting to switch'
    ]
  ])('%s', (_label, provider, state, words) => {
    expect(providerStateWords(provider, state)).toBe(words);
  });

  it('puts offline ahead of a selection in flight', () => {
    expect(
      providerStateWords(providerFixture(), { offline: true, selecting: true, unconfirmed: true })
    ).toBe('offline, selection unavailable');
  });
});

describe('retryOffered', () => {
  const failedWrite: MainProviderSelection = { provider: 'codex', restart_failed: false };

  it('is offered for a failed start, with no write failing', () => {
    expect(retryOffered(providerFixture({ status: 'failed' }), null)).toBe(true);
  });

  it('is offered for a pending switch with no write from this page', () => {
    expect(retryOffered(providerFixture({ status: 'pending' }), null)).toBe(true);
  });

  it('is offered after a failed write until a read shows it took effect', () => {
    expect(retryOffered(providerFixture(), failedWrite)).toBe(true);
  });

  it('is not offered once the failed write is confirmed ready', () => {
    expect(
      retryOffered(
        providerFixture({ desired_provider: 'codex', effective_provider: 'codex' }),
        failedWrite
      )
    ).toBe(false);
  });

  it('is not offered with no failure at all', () => {
    expect(retryOffered(providerFixture(), null)).toBe(false);
    expect(retryOffered(null, null)).toBe(false);
  });
});

describe('selectionNotice', () => {
  it('says a 409 was blocked, and that nothing is retried on its own', () => {
    const text = selectionNotice(new ApiError('Main provider is busy.', 409), 'codex');
    expect(text).toBe(
      'Provider change blocked (409): Main provider is busy. State is refreshed; no automatic retry.'
    );
  });

  it('says other failures were not confirmed', () => {
    const text = selectionNotice(new ApiError('Forbidden.', 403), 'codex');
    expect(text).toBe(
      'Provider selection was not confirmed: Forbidden. State is refreshed; no automatic retry.'
    );
  });

  it('treats a network failure as not confirmed, with a fallback when it has no message', () => {
    expect(selectionNotice(new TypeError('Failed to fetch'), 'codex')).toContain('Failed to fetch');
    expect(selectionNotice('oops', 'codex')).toContain('request failed');
  });

  it('bounds a long message', () => {
    const text = selectionNotice(new ApiError('x'.repeat(1000), 500), 'codex');
    expect(text).toContain('x'.repeat(NOTICE_MAX));
    expect(text).not.toContain('x'.repeat(NOTICE_MAX + 1));
  });
});

/** A 409 as the client builds it from a body with the contract's `code`. */
const coded = (code: RefusalCode, detail = 'Raw server prose.') => new ApiError(detail, 409, code);

describe('refusalCode', () => {
  it.each<RefusalCode>(['busy', 'pending', 'unconfirmed', 'unresolved', 'agent_unavailable'])(
    'reads the %s code',
    (code) => {
      expect(refusalCode(coded(code))).toBe(code);
    }
  );

  it.each([
    ['no code', new ApiError('Raw server prose.', 409)],
    ['a plain string', 'busy'],
    ['nothing', undefined]
  ])('gives no code for %s', (_label, error) => {
    expect(refusalCode(error)).toBeNull();
  });

  it('gives no code for a value the contract does not name', () => {
    // The client never passes one through; the list check is still the guard for any other caller.
    expect(refusalCode(coded('mystery' as RefusalCode))).toBeNull();
  });
});

describe('selectionNotice refusal codes', () => {
  it.each<[RefusalCode, string]>([
    [
      'busy',
      'Provider change refused: the main has open work or changed while the request ran. State is refreshed; no automatic retry.'
    ],
    [
      'pending',
      'Provider change refused: a switch is already pending and must settle or be recoverable first. State is refreshed; no automatic retry.'
    ],
    [
      'unconfirmed',
      'Provider change refused: kagent could not confirm the main is idle. State is refreshed; no automatic retry.'
    ],
    [
      'unresolved',
      'Provider change refused: a native create for the main is still unresolved. State is refreshed; no automatic retry.'
    ]
  ])('%s shows its own notice, not the server prose', (code, expected) => {
    expect(selectionNotice(coded(code), 'codex')).toBe(expected);
  });

  it('agent_unavailable names the provider asked for, and says nothing changed', () => {
    expect(selectionNotice(coded('agent_unavailable'), 'codex')).toBe(
      "Codex isn't installed here. Nothing changed. State is refreshed; no automatic retry."
    );
    expect(selectionNotice(coded('agent_unavailable'), 'claude')).toBe(
      "Claude isn't installed here. Nothing changed. State is refreshed; no automatic retry."
    );
  });

  it.each([
    ['an unknown code', new ApiError('Raw server prose.', 409, 'mystery' as RefusalCode)],
    ['no code', new ApiError('Raw server prose.', 409)]
  ])('falls back to the detail for %s', (_label, error) => {
    expect(selectionNotice(error, 'codex')).toBe(
      'Provider change blocked (409): Raw server prose. State is refreshed; no automatic retry.'
    );
  });

  it('a code wins over the status it arrived with', () => {
    const error = new ApiError('Forbidden.', 403, 'busy');
    expect(selectionNotice(error, 'codex')).toBe(
      'Provider change refused: the main has open work or changed while the request ran. State is refreshed; no automatic retry.'
    );
  });
});

describe('boundedText', () => {
  it('trims, then cuts to the bound', () => {
    expect(boundedText('  short  ')).toBe('short');
    expect(boundedText('y'.repeat(NOTICE_MAX + 50))).toHaveLength(NOTICE_MAX);
  });
});

describe('createRequestToken', () => {
  it('counts each request and only the newest is current', () => {
    const tokens = createRequestToken();
    const first = tokens.next();
    const second = tokens.next();
    expect(tokens.isLatest(first)).toBe(false);
    expect(tokens.isLatest(second)).toBe(true);
  });
});
