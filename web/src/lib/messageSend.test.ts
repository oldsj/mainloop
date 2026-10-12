import { expect, test } from 'vitest';
import { createSendGuard, retryMessage } from './messageSend';
import type { Message, NativeDelivery } from './contract';

const message: Message = {
  id: 'failed-main',
  conversation_id: 'main',
  role: 'user',
  content: 'repair this',
  created_at: '2026-10-06T00:00:00Z'
};
const failed: NativeDelivery = {
  message_id: message.id,
  task_id: null,
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
  expect(calls).toStrictEqual(['/messages:main']);
  expect(calls.includes(`/sessions/${selectedChild}/messages`)).toBe(false);
  // The composer remains an independent destination.
  await sendComposer();
  expect(calls.length).toBe(2);
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
    expect(posts).toBe(1);
    // A stale activity poll writes false while POST is unresolved.
    agentActive = false;
    expect(sendPending || agentActive).toBe(true);
    await click();
    expect(posts).toBe(1);
    post.resolve();
    await Promise.resolve();
    await click();
    expect(posts).toBe(1);
    expect(sendPending).toBe(true);
    refresh.resolve();
    await first;
    expect(sendPending).toBe(false);
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
    expect(posts).toBe(0);
  }
});

test('a failed eligibility refresh sends nothing and releases the local guard', async () => {
  let pending = false;
  let posts = 0;
  const guard = createSendGuard((value) => {
    pending = value;
  });
  await expect(
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
  ).rejects.toThrow();
  expect(posts).toBe(0);
  expect(pending).toBe(false);
});
