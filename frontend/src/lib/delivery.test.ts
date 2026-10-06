import assert from 'node:assert/strict';
import { test } from 'node:test';
import { deliveryNotices, threadStatus } from './delivery.ts';

const row = (message_id: string, state: string, detail: string | null = null, source = 'user') => ({
  message_id,
  state,
  detail,
  source,
  evidence_ref: null
});

test('a never-sent delivery is shown with its reason and can be retried', () => {
  const d = [row('m1', 'failed', 'not sent: SessionError: CreateSession failed (grpc 9)')];
  const n = deliveryNotices(d).get('m1');
  assert.equal(n?.label, 'Not sent');
  assert.match(n?.reason ?? '', /CreateSession failed/);
  assert.equal(n?.retryable, true);
});

test('a failed task is labelled failed, not not-sent', () => {
  const n = deliveryNotices([row('m1', 'failed', 'task failed: claude exited with an error')]).get(
    'm1'
  );
  assert.equal(n?.label, 'Failed');
});

test('only the newest delivery can be retried, and not while one is in flight', () => {
  const older = [row('m1', 'failed', 'not sent: x'), row('m2', 'completed')];
  assert.equal(deliveryNotices(older).get('m1')?.retryable, false);
  const busy = [row('m1', 'failed', 'not sent: x'), row('m2', 'sending')];
  assert.equal(deliveryNotices(busy).get('m1')?.retryable, false);
});

test('uncertain deliveries are flagged but never offered a retry', () => {
  const n = deliveryNotices([row('m1', 'uncertain', 'no task shows this message')]).get('m1');
  assert.equal(n?.state, 'uncertain');
  assert.equal(n?.retryable, false);
});

test('a report delivery is never retried as a user message', () => {
  const n = deliveryNotices([row('m1', 'failed', 'not sent: x', 'report')]).get('m1');
  assert.equal(n?.retryable, false);
});

test('completed and cancelled deliveries have no notice', () => {
  assert.equal(deliveryNotices([row('m1', 'completed'), row('m2', 'cancelled')]).size, 0);
});

test('status is not ready or working when the last delivery failed', () => {
  const failed = [row('m1', 'failed', 'not sent: x')];
  assert.equal(
    threadStatus({ offline: false, deliveries: failed, sessionState: 'ready' }),
    'failed'
  );
  assert.equal(
    threadStatus({ offline: false, deliveries: [row('m1', 'uncertain')], sessionState: 'ready' }),
    'unconfirmed'
  );
});

test('status recovers once a later delivery succeeds, and follows the turn state', () => {
  const recovered = [row('m1', 'failed', 'not sent: x'), row('m2', 'completed')];
  assert.equal(threadStatus({ offline: false, deliveries: recovered }), 'ready');
  assert.equal(threadStatus({ offline: false, deliveries: [row('m1', 'delivered')] }), 'working');
  assert.equal(threadStatus({ offline: false, deliveries: [], sessionState: 'suspended' }), 'idle');
  assert.equal(
    threadStatus({ offline: true, deliveries: [row('m1', 'delivered')] }),
    'unreachable'
  );
});
