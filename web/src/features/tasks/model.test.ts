import { describe, expect, it } from 'vitest';
import { attempt, CLAUDE_PROFILE } from '@/test/fixtures/tasks';
import { attemptSettings } from './model';

describe('attempt settings', () => {
  it('uses the attempt profile defaults independently for missing settings', () => {
    expect(attemptSettings(attempt({ model: 'custom-model' }), [CLAUDE_PROFILE])).toBe(
      ' · custom-model · medium effort'
    );
    expect(attemptSettings(attempt({ effort: 'high' }), [CLAUDE_PROFILE])).toBe(
      ' · claude-sonnet-5 · high effort'
    );
  });

  it('preserves explicit settings without a provider read', () => {
    expect(attemptSettings(attempt({ model: 'custom-model', effort: 'high' }), [])).toBe(
      ' · custom-model · high effort'
    );
  });

  it('does not invent defaults for an unknown profile or missing attempt', () => {
    expect(attemptSettings(attempt({ profile_id: 'missing' }), [CLAUDE_PROFILE])).toBe('');
    expect(attemptSettings(null, [CLAUDE_PROFILE])).toBe('');
  });
});
