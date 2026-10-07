import assert from 'node:assert/strict';
import { test } from 'node:test';
import { agentLabel } from './agentLabel.ts';

test('assistant prompts identify the native runtime', () => {
  assert.equal(agentLabel('codex'), 'codex');
  assert.equal(agentLabel('claude'), 'claude');
});

test('missing identity does not mislabel an agent as Claude', () => {
  assert.equal(agentLabel(null), 'agent');
  assert.equal(agentLabel(), 'agent');
});
