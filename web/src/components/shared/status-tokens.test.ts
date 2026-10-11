import { expect, test } from 'vitest';
import { SESSION_STATUS_TONE, sessionStatusTone } from './status-tokens';

test('each session status has the tone its owner needs to see', () => {
  expect(SESSION_STATUS_TONE).toStrictEqual({
    pending: 'active',
    active: 'active',
    idle: 'muted',
    superseded: 'muted',
    waiting_on_user: 'attention',
    completed: 'ok',
    failed: 'bad',
    cancelled: 'muted'
  });
});

test('statuses dropped in C1c fall back to muted instead of a tone of their own', () => {
  expect(sessionStatusTone('implementing')).toBe('muted');
  expect(sessionStatusTone('under_review')).toBe('muted');
  expect(sessionStatusTone('waiting_on_user')).toBe('attention');
});
