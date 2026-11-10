import { describe, expect, it } from 'vitest';
import { caretOnFirstLine, caretOnLastLine, sentHistory, step } from './history';
import type { Entry } from './store';

describe('composer history', () => {
  it('keeps user messages in order, once when repeated back to back', () => {
    const entries: Entry[] = [
      { t: 'user', id: 1, text: 'one', queued: false },
      { t: 'text', id: 2, text: 'reply' },
      { t: 'user', id: 3, text: 'two', queued: false },
      { t: 'user', id: 4, text: 'two', queued: false },
      { t: 'user', id: 5, text: 'three  (cancelled)', queued: false }
    ];
    expect(sentHistory(entries)).toEqual(['one', 'two', 'three']);
  });

  it('recalls only from the top and bottom line, with nothing selected', () => {
    expect(caretOnFirstLine('', 0, 0)).toBe(true);
    expect(caretOnFirstLine('a\nb', 1, 1)).toBe(true);
    expect(caretOnFirstLine('a\nb', 3, 3)).toBe(false);
    expect(caretOnFirstLine('abc', 0, 2)).toBe(false);
    expect(caretOnLastLine('a\nb', 3, 3)).toBe(true);
    expect(caretOnLastLine('a\nb', 1, 1)).toBe(false);
  });

  it('steps up to the oldest and down past the newest back to the draft', () => {
    expect(step(null, 0, 'up')).toBeUndefined();
    expect(step(null, 3, 'up')).toBe(2);
    expect(step(1, 3, 'up')).toBe(0);
    expect(step(0, 3, 'up')).toBeUndefined();
    expect(step(null, 3, 'down')).toBeUndefined();
    expect(step(1, 3, 'down')).toBe(2);
    expect(step(2, 3, 'down')).toBeNull();
  });
});
