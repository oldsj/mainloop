import { afterEach, describe, expect, it, vi } from 'vitest';
import { clearDraft, loadDraft, NEW_CONVERSATION, saveDraft } from './drafts';

afterEach(() => {
  sessionStorage.clear();
  vi.restoreAllMocks();
});

describe('drafts', () => {
  it('keeps one draft per conversation', () => {
    saveDraft('c1', 'first');
    saveDraft('c2', 'second');
    expect(loadDraft('c1')).toBe('first');
    expect(loadDraft('c2')).toBe('second');
    expect(loadDraft(NEW_CONVERSATION)).toBe('');
  });

  it('removes the entry when the draft is emptied or cleared', () => {
    saveDraft('c1', 'text');
    saveDraft('c1', '');
    expect(loadDraft('c1')).toBe('');
    saveDraft('c1', 'again');
    clearDraft('c1');
    expect(loadDraft('c1')).toBe('');
    expect(sessionStorage.length).toBe(0);
  });

  it('works without storage', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    expect(() => saveDraft('c1', 'x')).not.toThrow();
    expect(loadDraft('c1')).toBe('');
  });
});
