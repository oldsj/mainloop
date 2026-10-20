import { describe, expect, it } from 'vitest';
import { nativeFixture } from './fixtures';
import { describeModel, resolveModelCommand } from './modelCommand';
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
  models: [
    { id: 'claude-sonnet-5', label: 'Sonnet 5' },
    { id: 'claude-opus-5', label: 'Opus 5' }
  ],
  default_model: 'claude-sonnet-5',
  effort_levels: ['low', 'medium', 'high', 'xhigh', 'max'],
  default_effort: 'medium'
};

describe('resolveModelCommand', () => {
  it('with no args, describes the current model and the choices', () => {
    const native = nativeFixture({ model_override: null, model: 'claude-sonnet-5' });
    const outcome = resolveModelCommand([], native, claudeProfile);
    expect(outcome).toEqual({ notice: describeModel(native, claudeProfile) });
    expect('notice' in outcome && outcome.notice).toContain(
      'Current model: default (currently claude-sonnet-5).'
    );
    expect('notice' in outcome && outcome.notice).toContain(
      'Available: claude-sonnet-5, claude-opus-5, or default.'
    );
  });

  it('describes the overridden model by its label when the session has one', () => {
    const native = nativeFixture({ model_override: 'claude-opus-5' });
    expect(describeModel(native, claudeProfile)).toContain('Current model: Opus 5.');
  });

  it('sets a known model, matched case-insensitively against the profile', () => {
    const native = nativeFixture({ model_override: null });
    expect(resolveModelCommand(['Claude-Opus-5'], native, claudeProfile)).toEqual({
      model: 'claude-opus-5'
    });
  });

  it('accepts a model the profile does not list, since kagent is the real gate', () => {
    const native = nativeFixture({ model_override: null });
    expect(resolveModelCommand(['claude-fable-5-1'], native, claudeProfile)).toEqual({
      model: 'claude-fable-5-1'
    });
  });

  it('refuses a malformed model name without sending it', () => {
    const native = nativeFixture({ model_override: null });
    const outcome = resolveModelCommand(['bad name!'], native, claudeProfile);
    expect(outcome).toEqual({ notice: expect.stringContaining('is not a valid model name') });
  });

  it('reports a no-op instead of resetting when the model is already selected', () => {
    const native = nativeFixture({ model_override: 'claude-opus-5' });
    expect(resolveModelCommand(['claude-opus-5'], native, claudeProfile)).toEqual({
      notice: 'Already on Opus 5.'
    });
  });

  it('clears the override on "default"', () => {
    const native = nativeFixture({ model_override: 'claude-opus-5' });
    expect(resolveModelCommand(['default'], native, claudeProfile)).toEqual({ model: null });
  });

  it('reports a no-op for "default" when there is no override', () => {
    const native = nativeFixture({ model_override: null });
    expect(resolveModelCommand(['default'], native, claudeProfile)).toEqual({
      notice: 'Already on the default model.'
    });
  });

  it('refuses a second argument', () => {
    const native = nativeFixture({ model_override: null });
    const outcome = resolveModelCommand(['opus', 'extra'], native, claudeProfile);
    expect(outcome).toEqual({ notice: expect.stringContaining('Usage: /model <name>') });
  });
});
