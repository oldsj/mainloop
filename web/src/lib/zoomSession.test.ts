import { expect, test } from 'vitest';
import type { Session } from './contract';
import { statusLabel } from './sessionStatus';
import { zoomSession } from './zoomSession';

const original = { id: 'input', status: 'waiting_on_user', needs_owner: true } as Session;

test('zoom consumes settlement even if its conversation returns stale attention later', () => {
  const settled = { ...original, status: 'idle', needs_owner: false } as Session;
  const current = zoomSession('input', original, [settled], true, null);
  expect(current).toBe(settled);
  expect(statusLabel(current!.status, current!.needs_owner)).toBe('IDLE');
});

test('zoom consumes task finality and reassignment without reopening the view', () => {
  for (const status of ['completed', 'failed', 'cancelled', 'superseded']) {
    const current = { ...original, status, needs_owner: false } as Session;
    expect(zoomSession('input', original, [current], true, null)).toBe(current);
  }
});

test('missing sessions require a fresh detail read rather than old conversation attention', () => {
  expect(zoomSession('input', original, [], false, null)).toBe(original);
  expect(zoomSession('input', original, [], true, null)).toBe(null);
  const archived = {
    ...original,
    status: 'idle',
    needs_owner: false,
    archived_at: '2026-10-10'
  } as Session;
  expect(zoomSession('input', original, [], true, archived)).toBe(archived);
  expect(zoomSession('other', original, [], true, archived)).toBe(null);
});
