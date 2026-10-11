import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFile } from 'node:fs/promises';
import { transpileModule, ModuleKind } from 'typescript';
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

test('Chat keeps uncertain selection locked against a poll begun during selection until a fresh read', async () => {
  const source = await readFile(new URL('./components/Chat.svelte', import.meta.url), 'utf8');
  const start = source.indexOf('async function refreshMainThread()');
  const end = source.indexOf('let sendPending', start);
  assert.ok(start >= 0 && end > start);
  const handlers = transpileModule(source.slice(start, end), {
    compilerOptions: { module: ModuleKind.None }
  }).outputText;
  function deferred<T>() {
    let resolve!: (value: T) => void;
    let reject!: (reason: Error) => void;
    const promise = new Promise<T>((done, fail) => {
      resolve = done;
      reject = fail;
    });
    return { promise, resolve, reject };
  }
  const write = deferred<MainProviderInfo>();
  const oldPoll = deferred<MainThreadInfo>();
  const fresh: MainThreadInfo = {
    ...info,
    provider: { ...provider, desired_provider: 'codex', effective_provider: 'codex' }
  };
  let writes = 0;
  let gets = 0;
  const api = {
    setMainProvider: () => {
      writes++;
      return write.promise;
    },
    getMainThread: async () => {
      gets++;
      if (gets === 1) return oldPoll.promise;
      if (gets === 2) throw new Error('reconciliation unavailable');
      return fresh;
    }
  };
  const chat = new Function(
    'api',
    'mainReads',
    'providerReady',
    'selectMainProvider',
    'initial',
    `
    let mainThread = initial;
    let selectingProvider = false;
    let providerUnconfirmed = false;
    let providerError = null;
    let providerBlocked = false;
    ${handlers}
    return { changeMainProvider, refreshMainThread,
      snapshot: () => ({ mainThread, selectingProvider, providerUnconfirmed, providerError }) };
  `
  )(api, createMainThreadReads(), providerReady, selectMainProvider, info);

  const selection = chat.changeMainProvider('codex');
  const poll = chat.refreshMainThread();
  write.reject(new Error('PUT outcome unknown'));
  await selection;
  const failed = chat.snapshot();
  assert.equal(failed.selectingProvider, false);
  assert.equal(failed.providerUnconfirmed, true);
  assert.match(failed.providerError, /Waiting for a successful refresh/);

  // This Claude-ready observation predates the PUT, but arrives after failed settlement.
  oldPoll.resolve(info);
  await poll;
  assert.deepEqual(chat.snapshot(), failed);
  assert.equal(providerSwitchBlocked(failed.mainThread, failed.providerUnconfirmed), true);

  // A read begun after settlement may confirm the actual outcome and unlock recovery.
  await chat.refreshMainThread();
  const reconciled = chat.snapshot();
  assert.equal(reconciled.providerUnconfirmed, false);
  assert.equal(reconciled.providerError, null);
  assert.equal(reconciled.mainThread, fresh);
  assert.equal(providerSwitchBlocked(reconciled.mainThread, reconciled.providerUnconfirmed), false);
  await chat.changeMainProvider('codex');
  assert.equal(writes, 1);
  assert.equal(gets, 3);
});
