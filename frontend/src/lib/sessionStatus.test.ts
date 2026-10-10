import assert from 'node:assert/strict';
import test from 'node:test';
import { isCurrent, isFinished, statusLabel } from './sessionStatus.ts';

test('current sessions include running work and explicit owner attention', () => {
  for (const status of ['pending', 'active', 'implementing', 'under_review'])
    assert.equal(isCurrent({ status }), true);
  assert.equal(isCurrent({ status: 'idle', needs_owner: true }), true);
  assert.equal(isCurrent({ status: 'waiting_on_user', needs_owner: true }), true);
  assert.equal(isCurrent({ status: 'waiting_on_user', needs_owner: false }), false);
  assert.equal(isCurrent({ status: 'idle' }), false);
});

test('all finished outcomes stay behind history even with stale attention', () => {
  for (const status of ['completed', 'failed', 'cancelled', 'superseded']) {
    assert.equal(isFinished({ status }), true);
    assert.equal(isCurrent({ status, needs_owner: true }), false);
    assert.equal(statusLabel(status, true), statusLabel(status));
  }
});

test('idle availability and owner decisions have distinct labels', () => {
  assert.equal(statusLabel('idle'), 'IDLE');
  assert.equal(statusLabel('idle', true), 'NEEDS YOU');
  assert.equal(statusLabel('waiting_on_user', true), 'NEEDS INPUT');
  assert.equal(statusLabel('superseded'), 'SUPERSEDED');
});
