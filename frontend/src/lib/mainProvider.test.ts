import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  providerSwitchBlocked,
  providerReady,
  selectMainProvider,
  createMainThreadReads
} from './mainProvider.ts';
import type { MainThreadInfo, MainProviderInfo, NativeDelivery } from './api';
const provider: MainProviderInfo = {
  session_id: 'main',
  desired_provider: 'claude',
  effective_provider: 'claude',
  status: 'ready',
  kagent_session_id: 'native',
  error: null,
  recoverable: false,
  history: []
};
const info: MainThreadInfo = {
  mode: 'native',
  session_id: 'main',
  conversation_id: 'conversation',
  native: null,
  topics: [],
  provider
};

test('switch gates missing, pending, local busy, active turn and every unsettled delivery', () => {
  assert.equal(providerSwitchBlocked(info, false), false);
  assert.equal(providerSwitchBlocked(info, true), true);
  assert.equal(providerSwitchBlocked(null, false), true);
  assert.equal(providerSwitchBlocked({ ...info, provider: null }, false), true);
  assert.equal(
    providerSwitchBlocked({ ...info, provider: { ...provider, status: 'pending' } }, false),
    true
  );
  for (const state of ['recorded', 'sending', 'delivered', 'uncertain']) {
    const native = {
      turn_in_flight: false,
      deliveries: [{ state } as NativeDelivery]
    } as NonNullable<MainThreadInfo['native']>;
    assert.equal(providerSwitchBlocked({ ...info, native }, false), true, state);
  }
  const native = { turn_in_flight: true, deliveries: [] } as unknown as NonNullable<
    MainThreadInfo['native']
  >;
  assert.equal(providerSwitchBlocked({ ...info, native }, false), true);
  assert.equal(providerReady({ ...provider, effective_provider: 'codex' }), false);
  assert.equal(providerReady({ ...provider, status: 'failed' }), false);
});

test('uncertain write and conflict reconcile once without repeating PUT', async () => {
  for (const reason of ['network failure', '409 main_provider_busy']) {
    const calls: string[] = [];
    const errors: string[] = [];
    const confirmed = await selectMainProvider(
      'codex',
      false,
      async () => {
        calls.push('put');
        throw Object.assign(new Error(reason), { status: reason.startsWith('409') ? 409 : 0 });
      },
      async () => {
        calls.push('get');
      },
      (message) => errors.push(message)
    );
    assert.equal(confirmed, true);
    assert.deepEqual(calls, ['put', 'get']);
    assert.match(errors[0], new RegExp(reason));
  }
});

test('explicit restart passes permission; failed reconciliation remains unconfirmed', async () => {
  let writes = 0;
  const errors: string[] = [];
  assert.equal(
    await selectMainProvider(
      'claude',
      true,
      async (target, restart) => {
        writes++;
        assert.equal(target, 'claude');
        assert.equal(restart, true);
        return provider;
      },
      async () => {
        throw new Error('offline');
      },
      (message) => errors.push(message)
    ),
    false
  );
  assert.equal(writes, 1);
  assert.match(errors[0], /Waiting for a successful refresh/);
});

test('selection invalidation and newer reads prevent stale poll results overwriting state', async () => {
  const reads = createMainThreadReads();
  let resolve!: (value: MainThreadInfo) => void;
  const old = new Promise<MainThreadInfo>((done) => {
    resolve = done;
  });
  const applied: MainThreadInfo[] = [];
  const poll = reads.read(
    () => old,
    (value) => applied.push(value)
  );
  reads.invalidate();
  const fresh = {
    ...info,
    provider: {
      ...provider,
      desired_provider: 'codex' as const,
      effective_provider: 'codex' as const
    }
  };
  await reads.read(
    async () => fresh,
    (value) => applied.push(value)
  );
  resolve(info);
  await poll;
  assert.deepEqual(applied, [fresh]);
});
