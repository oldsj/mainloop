import { describe, expect, it } from 'vitest';
import { fieldText, runDefaults, runNotes, sourceWords } from './runDefaults';

const workerOpus = [
  { role: 'worker' as const, provider: 'claude', model: 'claude-opus-5', effort: 'high' as const }
];

describe('where a session’s model and effort come from', () => {
  it('is the session’s own choice when it has one', () => {
    const run = runDefaults(
      { kind: 'claude', model_override: 'claude-sonnet-5', effort_override: 'low' },
      workerOpus,
      'worker'
    );
    expect(run).toEqual({
      model: { value: 'claude-sonnet-5', source: 'session' },
      effort: { value: 'low', source: 'session' }
    });
  });

  it('is the owner’s default for the role and provider when the session has none', () => {
    const run = runDefaults({ kind: 'claude' }, workerOpus, 'worker');
    expect(run).toEqual({
      model: { value: 'claude-opus-5', source: 'role' },
      effort: { value: 'high', source: 'role' }
    });
  });

  it('is the provider’s when there is neither', () => {
    const none = { value: null, source: 'provider' };
    expect(runDefaults({ kind: 'claude' }, [], 'worker')).toEqual({ model: none, effort: none });
    expect(runDefaults({ kind: 'claude' }, undefined, 'worker').model).toEqual(none);
  });

  it('never applies one role’s default to the other, or one provider’s to another', () => {
    expect(runDefaults({ kind: 'claude' }, workerOpus, 'main').model.source).toBe('provider');
    expect(runDefaults({ kind: 'codex' }, workerOpus, 'worker').model.source).toBe('provider');
  });

  it('resolves each field on its own', () => {
    const run = runDefaults(
      { kind: 'claude', model_override: 'claude-sonnet-5' },
      workerOpus,
      'worker'
    );
    expect(run.model.source).toBe('session');
    expect(run.effort).toEqual({ value: 'high', source: 'role' });
  });

  it('reads a default effort the provider stopped offering as the provider’s, as the turn does', () => {
    const run = runDefaults({ kind: 'claude' }, workerOpus, 'worker', ['low', 'medium']);
    expect(run.effort).toEqual({ value: null, source: 'provider' });
    expect(run.model.source).toBe('role');
    // With the provider's levels unknown the default is taken as offered.
    expect(runDefaults({ kind: 'claude' }, workerOpus, 'worker').effort.source).toBe('role');
  });
});

describe('how the composer says it', () => {
  it('names each source', () => {
    expect(sourceWords('session', 'worker')).toBe('this session');
    expect(sourceWords('role', 'worker')).toBe('worker default');
    expect(sourceWords('role', 'main')).toBe('main default');
    expect(sourceWords('provider', 'main')).toBe('provider default');
  });

  it('gives the chip one note when both parts come from one place', () => {
    expect(runNotes(runDefaults({ kind: 'claude' }, workerOpus, 'worker'), 'worker')).toEqual({
      all: 'worker default'
    });
  });

  it('says nothing when both are the provider’s own, which is what an unmarked chip means', () => {
    expect(runNotes(runDefaults({ kind: 'claude' }, [], 'main'), 'main')).toEqual({});
  });

  it('notes only the part that is not the provider’s when one of them is', () => {
    const modelOnly = [
      { role: 'main' as const, provider: 'claude', model: 'claude-opus-5', effort: null }
    ];
    expect(runNotes(runDefaults({ kind: 'claude' }, modelOnly, 'main'), 'main')).toEqual({
      model: 'main default'
    });
  });

  it('gives each part its own note when they differ', () => {
    const run = runDefaults(
      { kind: 'claude', model_override: 'claude-sonnet-5' },
      workerOpus,
      'worker'
    );
    expect(runNotes(run, 'worker')).toEqual({ model: 'this session', effort: 'worker default' });
  });

  it('writes the chat composer’s line: bare for the session, noted for a role default, "default" for the provider', () => {
    expect(fieldText({ value: 'claude-opus-5', source: 'session' }, 'main')).toBe('claude-opus-5');
    expect(fieldText({ value: 'claude-opus-5', source: 'role' }, 'main')).toBe(
      'claude-opus-5 (main default)'
    );
    expect(fieldText({ value: 'high', source: 'role' }, 'worker')).toBe('high (worker default)');
    expect(fieldText({ value: null, source: 'provider' }, 'worker')).toBe('default');
  });
});
