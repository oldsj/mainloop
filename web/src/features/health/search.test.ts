import { describe, expect, it } from 'vitest';
import { healthSearch } from './search';

describe('healthSearch', () => {
  it('reads the URL values, with the window coerced from text', () => {
    expect(healthSearch.parse({ task_id: 'task-1', hours: '168', provider: 'codex' })).toEqual({
      task_id: 'task-1',
      hours: 168,
      provider: 'codex'
    });
  });

  it('drops an unreadable value to its default instead of failing', () => {
    expect(healthSearch.parse({ hours: 'abc', provider: 'gpt', task_id: 3 })).toEqual({
      hours: undefined,
      provider: undefined,
      task_id: undefined
    });
  });

  it('accepts only the four windows the server offers', () => {
    expect(healthSearch.parse({ hours: '720' }).hours).toBe(720);
    expect(healthSearch.parse({ hours: '5' }).hours).toBeUndefined();
    expect(healthSearch.parse({ hours: '-24' }).hours).toBeUndefined();
  });
});
