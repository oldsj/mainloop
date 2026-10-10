import assert from 'node:assert/strict';
import test from 'node:test';
import type { Session } from './api';
import { statusLabel } from './sessionStatus.ts';
import { zoomSession } from './zoomSession.ts';

const original = { id: 'input', status: 'waiting_on_user', needs_owner: true } as Session;

test('zoom consumes settlement even if its conversation returns stale attention later', () => {
  const settled = { ...original, status: 'idle', needs_owner: false } as Session;
  const current = zoomSession('input', original, [settled], true, null);
  assert.equal(current, settled);
  assert.equal(statusLabel(current!.status, current!.needs_owner), 'IDLE');
});

test('zoom consumes task finality and reassignment without reopening the view', () => {
  for (const status of ['completed', 'failed', 'cancelled', 'superseded']) {
    const current = { ...original, status, needs_owner: false } as Session;
    assert.equal(zoomSession('input', original, [current], true, null), current);
  }
});

test('missing sessions require a fresh detail read rather than old conversation attention', () => {
  assert.equal(zoomSession('input', original, [], false, null), original);
  assert.equal(zoomSession('input', original, [], true, null), null);
  const archived = {
    ...original,
    status: 'idle',
    needs_owner: false,
    archived_at: '2026-10-10'
  } as Session;
  assert.equal(zoomSession('input', original, [], true, archived), archived);
  assert.equal(zoomSession('other', original, [], true, archived), null);
});
