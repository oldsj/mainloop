import { expect, test } from 'vitest';
import { agentLabel } from './agentLabel';

test('assistant prompts identify the native runtime', () => {
  expect(agentLabel('codex')).toBe('codex');
  expect(agentLabel('claude')).toBe('claude');
});

test('missing identity does not mislabel an agent as Claude', () => {
  expect(agentLabel(null)).toBe('agent');
  expect(agentLabel()).toBe('agent');
});
