import { expect, test } from 'vitest';
import { isCurrent, isFinished, statusLabel } from './sessionStatus';

test('current sessions include running work and explicit owner attention', () => {
  for (const status of ['pending', 'active']) expect(isCurrent({ status })).toBe(true);
  expect(isCurrent({ status: 'idle', needs_owner: true })).toBe(true);
  expect(isCurrent({ status: 'waiting_on_user', needs_owner: true })).toBe(true);
  expect(isCurrent({ status: 'waiting_on_user', needs_owner: false })).toBe(false);
  expect(isCurrent({ status: 'idle' })).toBe(false);
});

test('all finished outcomes stay behind history even with stale attention', () => {
  for (const status of ['completed', 'failed', 'cancelled', 'superseded']) {
    expect(isFinished({ status })).toBe(true);
    expect(isCurrent({ status, needs_owner: true })).toBe(false);
    expect(statusLabel(status, true)).toBe(statusLabel(status));
  }
});

test('idle availability and owner decisions have distinct labels', () => {
  expect(statusLabel('idle')).toBe('IDLE');
  expect(statusLabel('idle', true)).toBe('NEEDS YOU');
  expect(statusLabel('waiting_on_user', true)).toBe('NEEDS INPUT');
  expect(statusLabel('superseded')).toBe('SUPERSEDED');
});
