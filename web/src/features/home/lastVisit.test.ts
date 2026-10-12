import { afterEach, describe, expect, it, vi } from 'vitest';
import { readLastVisit, writeLastVisit } from './lastVisit';

afterEach(() => {
  window.localStorage.clear();
  vi.restoreAllMocks();
});

describe('last visit', () => {
  it('reads back what was written', () => {
    writeLastVisit(1_700_000_000_000);
    expect(readLastVisit()).toBe(1_700_000_000_000);
  });

  it('reports no visit when nothing is held or the value is not a time', () => {
    expect(readLastVisit()).toBeNull();
    window.localStorage.setItem('mainloop.home.lastVisit', 'yesterday');
    expect(readLastVisit()).toBeNull();
  });

  it('does not throw when storage is blocked', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    expect(readLastVisit()).toBeNull();
    expect(() => writeLastVisit()).not.toThrow();
  });
});
