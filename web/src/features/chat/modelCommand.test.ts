import { describe, expect, it } from 'vitest';
import { providerFixture, mainThreadFixture } from './fixtures';
import { describeModel, EFFORT_UNSUPPORTED_NOTICE, resolveModelCommand } from './modelCommand';

describe('resolveModelCommand', () => {
  it('with no args, describes the current model, the choices, and that effort is unsupported', () => {
    const info = mainThreadFixture({ provider: providerFixture({ desired_provider: 'claude' }) });
    const outcome = resolveModelCommand([], info);
    expect(outcome).toEqual({ notice: describeModel(info) });
    expect('notice' in outcome && outcome.notice).toContain('Current model: Claude (ready).');
    expect('notice' in outcome && outcome.notice).toContain('Available: claude, codex.');
    expect('notice' in outcome && outcome.notice).toContain(EFFORT_UNSUPPORTED_NOTICE);
  });

  it('describes an unread main thread without claiming a current provider', () => {
    expect(describeModel(null)).toContain('Current model: unknown (main thread not read yet).');
  });

  it('switches to a known, different provider, case-insensitively', () => {
    const info = mainThreadFixture({ provider: providerFixture({ desired_provider: 'claude' }) });
    expect(resolveModelCommand(['Codex'], info)).toEqual({ provider: 'codex' });
  });

  it('reports a no-op instead of switching when the provider is already selected and ready', () => {
    const info = mainThreadFixture({
      provider: providerFixture({ desired_provider: 'claude', effective_provider: 'claude' })
    });
    expect(resolveModelCommand(['claude'], info)).toEqual({ notice: 'Already on Claude.' });
  });

  it('reports an unknown provider rather than switching', () => {
    const info = mainThreadFixture();
    const outcome = resolveModelCommand(['opus-5.5'], info);
    expect(outcome).toEqual({
      notice: 'Unknown model "opus-5.5". Available: claude, codex. Usage: /model <name>.'
    });
  });

  it('refuses a second argument outright instead of silently discarding a reasoning-effort choice', () => {
    const info = mainThreadFixture();
    expect(resolveModelCommand(['codex', 'medium'], info)).toEqual({
      notice: EFFORT_UNSUPPORTED_NOTICE
    });
  });
});
