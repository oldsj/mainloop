import { describe, expect, it } from 'vitest';
import { profile } from '@/test/fixtures/tasks';
import { schedule } from './test/fixtures';
import {
  agentOptions,
  createRequestIds,
  describeCron,
  draftFromConfig,
  emptyDraft,
  executionStateLabel,
  isValidTimeZone,
  normalizeTimeZone,
  validateDraft,
  type ScheduleDraft
} from './model';

const valid = (overrides: Partial<ScheduleDraft> = {}): ScheduleDraft => ({
  ...emptyDraft('Europe/London'),
  agent: 'agents/triage',
  prompt: 'Triage the inbox',
  schedule: '0 2 * * *',
  ...overrides
});

describe('cron preview', () => {
  it.each([
    ['0 2 * * *', 'Every day at 02:00'],
    ['*/15 * * * *', 'Every 15 minutes'],
    ['30 * * * *', 'Every hour, at minute 30'],
    ['0 */6 * * *', 'Every 6 hours, at minute 0'],
    ['30 9 * * 1', 'Every Monday at 09:30'],
    ['0 9 * * 1-5', 'Every weekday (Monday to Friday) at 09:00'],
    ['0 0 1 * *', 'On day 1 of every month at 00:00'],
    ['0 0 25 12 *', 'Every year on December 25 at 00:00']
  ])('%s reads as %j', (expression, text) => {
    expect(describeCron(expression)).toEqual({ ok: true, text });
  });

  it('falls back to a field-by-field reading for shapes it does not name', () => {
    const preview = describeCron('0 2 * * 1,3');
    expect(preview.ok).toBe(true);
    expect(preview.ok && preview.text.startsWith('Custom:')).toBe(true);
  });

  it.each([
    ['', 'Enter a five-field schedule.'],
    ['@daily', 'Macros such as @daily are not supported. Use five fields.'],
    ['0 2 * *', 'Use five fields: minute, hour, day of month, month, day of week. Found 4.'],
    ['99 * * * *', null],
    ['0 2 * * 7', null]
  ])('rejects %j', (expression, message) => {
    const preview = describeCron(expression);
    expect(preview.ok).toBe(false);
    if (message && !preview.ok) expect(preview.message).toBe(message);
  });
});

describe('draft validation', () => {
  it('accepts a complete draft and fills the contract defaults', () => {
    const result = validateDraft(valid({ timeZone: '  ', timeoutSeconds: '900' }));
    expect(result).toEqual({
      ok: true,
      config: {
        name: '',
        schedule: '0 2 * * *',
        time_zone: 'UTC',
        prompt: 'Triage the inbox',
        paused: false,
        execution_timeout_seconds: 900
      }
    });
  });

  it('keys each problem by the contract field name', () => {
    const result = validateDraft(
      valid({ schedule: '99 * * * *', timeZone: 'Local', prompt: '   ', timeoutSeconds: '0' })
    );
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(Object.keys(result.errors).sort()).toEqual([
      'execution_timeout_seconds',
      'prompt',
      'schedule',
      'time_zone'
    ]);
  });

  it('rejects a prompt over the byte limit even when its character count is under it', () => {
    const result = validateDraft(valid({ prompt: 'é'.repeat(20_000) }));
    expect(!result.ok && result.errors.prompt).toMatch(/over 32768 bytes/);
  });

  it('rejects blank, zero and sub-contract timeouts, and accepts the contract minimum', () => {
    expect(validateDraft(valid({ timeoutSeconds: '' })).ok).toBe(false);
    expect(validateDraft(valid({ timeoutSeconds: '0' })).ok).toBe(false);
    expect(validateDraft(valid({ timeoutSeconds: '0.0000001' })).ok).toBe(false);
    expect(validateDraft(valid({ timeoutSeconds: '0.000001' })).ok).toBe(true);
  });

  it('reads a saved schedule back into a draft that is still valid', () => {
    const draft = draftFromConfig(schedule().config);
    expect(validateDraft({ ...draft, agent: 'agents/triage' }).ok).toBe(true);
  });
});

describe('time zones', () => {
  it('accepts IANA names and rejects Local and unknown names', () => {
    expect(isValidTimeZone('Europe/London')).toBe(true);
    expect(isValidTimeZone('Local')).toBe(false);
    expect(isValidTimeZone('Mars/Olympus')).toBe(false);
  });

  it('treats a blank zone as UTC', () => {
    expect(normalizeTimeZone('   ')).toBe('UTC');
    expect(normalizeTimeZone(' Asia/Tokyo ')).toBe('Asia/Tokyo');
  });
});

describe('agent choices', () => {
  it('lists each enabled role once, under the first provider that names it', () => {
    const claude = profile({
      id: 'claude-default',
      display_name: 'Claude',
      native_provider: 'claude',
      agents: { worker: { namespace: 'agents', name: 'triage' } }
    });
    const disabled = profile({
      id: 'codex-off',
      display_name: 'Codex',
      native_provider: 'codex',
      enabled: false,
      agents: { worker: { namespace: 'agents', name: 'hidden' } }
    });
    const codex = profile({
      id: 'codex-default',
      display_name: 'Codex',
      native_provider: 'codex',
      agents: {
        worker: { namespace: 'agents', name: 'triage' },
        lead: { namespace: 'agents', name: 'lead' }
      }
    });
    const options = agentOptions([claude, disabled, codex]);
    expect(options.map((option) => option.value)).toEqual(['agents/triage', 'agents/lead']);
    expect(options[0]?.label).toBe('Claude, worker: triage');
  });
});

describe('idempotency keys', () => {
  it('reuses one id for the same request and mints a new one when the request changes', () => {
    let minted = 0;
    const ids = createRequestIds(() => `id-${++minted}`);
    expect(ids.idFor('a')).toBe('id-1');
    expect(ids.idFor('a')).toBe('id-1');
    expect(ids.idFor('b')).toBe('id-2');
  });

  it('mints a fresh id after reset', () => {
    let minted = 0;
    const ids = createRequestIds(() => `id-${++minted}`);
    expect(ids.idFor('a')).toBe('id-1');
    ids.reset();
    expect(ids.idFor('a')).toBe('id-2');
  });
});

it('labels every execution state in words', () => {
  expect(executionStateLabel('timed_out')).toBe('Timed out');
  expect(executionStateLabel('pending')).toBe('Pending');
});
