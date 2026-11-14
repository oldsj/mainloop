import { describe, expect, it } from 'vitest';
import { CLAUDE_PROFILE, CODEX_PROFILE } from '@/features/chat/handlers';
import {
  defaultFor,
  effortChoices,
  effortStale,
  modelChoices,
  modelLabel,
  providerDefaultLabel,
  ROLES,
  selectableProfiles
} from './model';

describe('session defaults form rules', () => {
  it('lists the main thread, then workers', () => {
    expect(ROLES.map((role) => role.id)).toEqual(['main', 'worker']);
  });

  it('reads the default for a role and provider, or one left to the provider', () => {
    const defaults = [
      { role: 'worker' as const, provider: 'claude', model: 'claude-opus-5', effort: null }
    ];
    expect(defaultFor(defaults, 'worker', 'claude')).toEqual(defaults[0]);
    expect(defaultFor(defaults, 'main', 'claude')).toEqual({
      role: 'main',
      provider: 'claude',
      model: null,
      effort: null
    });
    expect(defaultFor(undefined, 'worker', 'codex').model).toBeNull();
  });

  it('offers only enabled profiles', () => {
    const off = { ...CODEX_PROFILE, enabled: false };
    expect(selectableProfiles([CLAUDE_PROFILE, off]).map((profile) => profile.id)).toEqual([
      'claude'
    ]);
    expect(selectableProfiles(undefined)).toEqual([]);
  });

  it('names the provider default, with the profile’s own default as a hint', () => {
    expect(providerDefaultLabel('Sonnet 5')).toBe('Provider default (Sonnet 5)');
    expect(providerDefaultLabel(null)).toBe('Provider default');
    expect(modelLabel(CLAUDE_PROFILE, 'claude-sonnet-5')).toBe('Sonnet 5');
    expect(modelLabel(CLAUDE_PROFILE, 'something-else')).toBe('something-else');
    expect(modelLabel(CLAUDE_PROFILE, null)).toBeNull();
  });

  it('keeps a saved model the profile does not list among the choices', () => {
    expect(modelChoices(CLAUDE_PROFILE, 'claude-opus-5')).toHaveLength(
      CLAUDE_PROFILE.models.length
    );
    const choices = modelChoices(CLAUDE_PROFILE, 'custom-model');
    expect(choices.at(-1)).toEqual({
      id: 'custom-model',
      label: "custom-model (not in this profile's list)"
    });
  });

  it('keeps a saved effort the profile stopped offering, and flags it', () => {
    expect(effortChoices(CODEX_PROFILE, 'high').map((choice) => choice.id)).toEqual(
      CODEX_PROFILE.effort_levels
    );
    expect(effortStale(CODEX_PROFILE, 'high')).toBe(false);
    // Codex's built-in profile has no "max".
    expect(effortChoices(CODEX_PROFILE, 'max').at(-1)).toEqual({
      id: 'max',
      label: 'max (no longer offered)'
    });
    expect(effortStale(CODEX_PROFILE, 'max')).toBe(true);
    expect(effortStale(CODEX_PROFILE, null)).toBe(false);
  });
});
