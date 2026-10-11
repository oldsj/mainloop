import { expect, test } from 'vitest';
import { deliveryNotices, threadStatus } from './delivery';

const row = (message_id: string, state: string, detail: string | null = null, source = 'user') => ({
  message_id,
  task_id: null,
  state,
  detail,
  source,
  evidence_ref: null
});

test('a never-sent delivery is shown with its reason and can be retried', () => {
  const d = [row('m1', 'failed', 'not sent: SessionError: CreateSession failed (grpc 9)')];
  const n = deliveryNotices(d).get('m1');
  expect(n?.label).toBe('Not sent');
  expect(n?.reason ?? '').toMatch(/CreateSession failed/);
  expect(n?.retryable).toBe(true);
});

test('a failed task is labelled failed, not not-sent', () => {
  const n = deliveryNotices([row('m1', 'failed', 'task failed: claude exited with an error')]).get(
    'm1'
  );
  expect(n?.label).toBe('Failed');
});

test('only the newest delivery can be retried, and not while one is in flight', () => {
  const older = [row('m1', 'failed', 'not sent: x'), row('m2', 'completed')];
  expect(deliveryNotices(older).get('m1')?.retryable).toBe(false);
  const busy = [row('m1', 'failed', 'not sent: x'), row('m2', 'sending')];
  expect(deliveryNotices(busy).get('m1')?.retryable).toBe(false);
});

test('uncertain deliveries are flagged but never offered a retry', () => {
  const n = deliveryNotices([row('m1', 'uncertain', 'no task shows this message')]).get('m1');
  expect(n?.state).toBe('uncertain');
  expect(n?.retryable).toBe(false);
});

test('a report delivery is never retried as a user message', () => {
  const n = deliveryNotices([row('m1', 'failed', 'not sent: x', 'report')]).get('m1');
  expect(n?.retryable).toBe(false);
});

test('completed and cancelled deliveries have no notice', () => {
  expect(deliveryNotices([row('m1', 'completed'), row('m2', 'cancelled')]).size).toBe(0);
});

test('status is not ready or working when the last delivery failed', () => {
  const failed = [row('m1', 'failed', 'not sent: x')];
  expect(threadStatus({ offline: false, deliveries: failed, sessionState: 'ready' })).toBe(
    'failed'
  );
  expect(
    threadStatus({ offline: false, deliveries: [row('m1', 'uncertain')], sessionState: 'ready' })
  ).toBe('unconfirmed');
});

test('status recovers once a later delivery succeeds, and follows the turn state', () => {
  const recovered = [row('m1', 'failed', 'not sent: x'), row('m2', 'completed')];
  expect(threadStatus({ offline: false, deliveries: recovered })).toBe('ready');
  expect(threadStatus({ offline: false, deliveries: [row('m1', 'delivered')] })).toBe('working');
  expect(threadStatus({ offline: false, deliveries: [], sessionState: 'suspended' })).toBe('idle');
  expect(threadStatus({ offline: true, deliveries: [row('m1', 'delivered')] })).toBe('unreachable');
});
