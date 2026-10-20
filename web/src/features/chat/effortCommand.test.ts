import { describe, expect, it } from 'vitest';
import { nativeFixture } from './fixtures';
import { describeEffort, resolveEffortCommand } from './effortCommand';
import type { ProviderProfile } from './types';

const claudeProfile: ProviderProfile = {
  id: 'claude',
  display_name: 'Claude',
  runtime_adapter: 'kagent',
  native_provider: 'claude',
  configuration_revision: 'r1',
  agents: {},
  aliases: [],
  enabled: true,
  capabilities: [],
  models: [],
  default_model: null,
  effort_levels: ['low', 'medium', 'high', 'xhigh', 'max'],
  default_effort: 'medium'
};

const codexProfile: ProviderProfile = {
  ...claudeProfile,
  id: 'codex',
  native_provider: 'codex',
  effort_levels: ['minimal', 'low', 'medium', 'high']
};

describe('resolveEffortCommand', () => {
  it('with no args, describes the current effort and the choices', () => {
    const native = nativeFixture({ effort_override: null });
    const outcome = resolveEffortCommand([], native, claudeProfile);
    expect(outcome).toEqual({ notice: describeEffort(native, claudeProfile) });
    expect('notice' in outcome && outcome.notice).toContain('Current effort: default.');
    expect('notice' in outcome && outcome.notice).toContain(
      'Available: low, medium, high, xhigh, max, or default.'
    );
  });

  it('describes the overridden effort', () => {
    const native = nativeFixture({ effort_override: 'xhigh' });
    expect(describeEffort(native, claudeProfile)).toContain('Current effort: xhigh.');
  });

  it('sets a level the provider offers, case-insensitively', () => {
    const native = nativeFixture({ effort_override: null });
    expect(resolveEffortCommand(['XHIGH'], native, claudeProfile)).toEqual({ effort: 'xhigh' });
  });

  it('rejects a level the current provider does not offer', () => {
    const native = nativeFixture({ effort_override: null });
    expect(resolveEffortCommand(['xhigh'], native, codexProfile)).toEqual({
      notice: expect.stringContaining('Unknown effort "xhigh"')
    });
  });

  it('accepts minimal for Codex', () => {
    const native = nativeFixture({ effort_override: null });
    expect(resolveEffortCommand(['minimal'], native, codexProfile)).toEqual({ effort: 'minimal' });
  });

  it('reports a no-op instead of resetting when the level is already selected', () => {
    const native = nativeFixture({ effort_override: 'high' });
    expect(resolveEffortCommand(['high'], native, claudeProfile)).toEqual({
      notice: 'Already on high.'
    });
  });

  it('clears the override on "default"', () => {
    const native = nativeFixture({ effort_override: 'high' });
    expect(resolveEffortCommand(['default'], native, claudeProfile)).toEqual({ effort: null });
  });

  it('reports a no-op for "default" when there is no override', () => {
    const native = nativeFixture({ effort_override: null });
    expect(resolveEffortCommand(['default'], native, claudeProfile)).toEqual({
      notice: 'Already on the default effort.'
    });
  });

  it('refuses a second argument', () => {
    const native = nativeFixture({ effort_override: null });
    const outcome = resolveEffortCommand(['high', 'extra'], native, claudeProfile);
    expect(outcome).toEqual({ notice: expect.stringContaining('Usage: /effort <level>') });
  });
});
