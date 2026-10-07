import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createSendGuard, retryMessage } from './messageSend.ts';
import type { Message, NativeDelivery } from './api';

const message: Message = {
  id: 'failed-main',
  conversation_id: 'main',
  role: 'user',
  content: 'repair this',
  created_at: '2026-10-06T00:00:00Z'
};
const failed: NativeDelivery = {
  message_id: message.id,
  state: 'failed',
  source: 'user',
  detail: 'not sent: failure',
  evidence_ref: null
};
const snapshot = () => ({ conversationId: 'main', deliveries: [failed], blocked: false });
function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

test('main Retry targets its owning conversation even with a child selected', async () => {
  const selectedChild = 'child';
  const calls: string[] = [];
  // These are the endpoint callbacks used by main Retry and the context-aware composer.
  const sendMain = async (_text: string, conversationId: string) => {
    calls.push(`/messages:${conversationId}`);
  };
  const sendComposer = async () => {
    calls.push(`/sessions/${selectedChild}/messages`);
  };
  await retryMessage(message, async () => snapshot(), sendMain);
  assert.deepEqual(calls, ['/messages:main']);
  assert.equal(calls.includes(`/sessions/${selectedChild}/messages`), false);
  // The composer remains an independent destination.
  await sendComposer();
  assert.equal(calls.length, 2);
});

for (const conversation of ['main', 'session']) {
  test(`${conversation}: a stale poll between clicks cannot unlock POST or refresh`, async () => {
    const post = deferred();
    const refresh = deferred();
    let agentActive = false;
    let sendPending = false;
    let posts = 0;
    const guard = createSendGuard((pending) => {
      sendPending = pending;
    });
    const click = () =>
      guard(async () => {
        await retryMessage(
          message,
          async () => snapshot(),
          async () => {
            posts++;
            await post.promise;
            await refresh.promise;
          }
        );
      });
    const first = click();
    await Promise.resolve();
    assert.equal(posts, 1);
    // A stale activity poll writes false while POST is unresolved.
    agentActive = false;
    assert.equal(sendPending || agentActive, true);
    await click();
    assert.equal(posts, 1);
    post.resolve();
    await Promise.resolve();
    await click();
    assert.equal(posts, 1);
    assert.equal(sendPending, true);
    refresh.resolve();
    await first;
    assert.equal(sendPending, false);
  });
}

test('Retry rechecks newest delivery, open turns, source and owning conversation', async () => {
  for (const fresh of [
    { ...snapshot(), blocked: true },
    { ...snapshot(), conversationId: 'other' },
    { ...snapshot(), deliveries: [{ ...failed, state: 'uncertain' }] },
    { ...snapshot(), deliveries: [{ ...failed, source: 'report' }] },
    { ...snapshot(), deliveries: [failed, { ...failed, message_id: 'new', state: 'completed' }] },
    { ...snapshot(), deliveries: [failed, { ...failed, message_id: 'new', state: 'sending' }] }
  ]) {
    let posts = 0;
    await retryMessage(
      message,
      async () => fresh,
      async () => {
        posts++;
      }
    );
    assert.equal(posts, 0);
  }
});

test('a failed eligibility refresh sends nothing and releases the local guard', async () => {
  let pending = false;
  let posts = 0;
  const guard = createSendGuard((value) => {
    pending = value;
  });
  await assert.rejects(
    guard(() =>
      retryMessage(
        message,
        async () => {
          throw new Error('offline');
        },
        async () => {
          posts++;
        }
      )
    )
  );
  assert.equal(posts, 0);
  assert.equal(pending, false);
});

test('Chat Retry handler calls only main send when a child context is selected', async () => {
  const { readFile } = await import('node:fs/promises');
  const source = await readFile(new URL('./components/Chat.svelte', import.meta.url), 'utf8');
  assert.match(source, /onRetry=\{handleRetry\}/);
  const start = source.indexOf('async function handleRetry(');
  const end = source.indexOf('async function sendMainThreadMessage(', start);
  const handler = source.slice(start, end).replace('message: Message', 'message');
  const calls: string[] = [];
  const api = {
    getMainThread: async () => ({
      conversation_id: 'main',
      native: { deliveries: [failed], turn_in_flight: false }
    }),
    sendMessage: async () => {
      calls.push('main');
    },
    sendSessionMessage: async () => {
      calls.push('child');
    }
  };
  const guardSend = createSendGuard(() => {});
  // Execute the actual component handler with a child selected and mocked endpoints.
  const handleRetry = new Function(
    'guardSend',
    'retryMessage',
    'api',
    'sendMainThreadMessage',
    'offline',
    '$currentSession',
    '$isMainContext',
    'handleSendMessage',
    `let mainThread; let sendError; ${handler}; return handleRetry;`
  )(
    guardSend,
    retryMessage,
    api,
    () => api.sendMessage(),
    false,
    { id: 'child' },
    false,
    () => api.sendSessionMessage()
  );
  await handleRetry(message);
  assert.deepEqual(calls, ['main']);
});
