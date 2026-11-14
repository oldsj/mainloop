import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  DEFAULT_PREFS,
  PREFS_KEY,
  WIDTH_DEFAULT,
  WIDTH_MAX,
  WIDTH_MIN,
  clampWidth,
  isExpanded,
  readPrefs,
  writePrefs
} from './prefs';

afterEach(() => {
  window.localStorage.clear();
  vi.restoreAllMocks();
});

describe('width', () => {
  it('keeps a dragged width inside the range', () => {
    expect(clampWidth(10)).toBe(WIDTH_MIN);
    expect(clampWidth(9999)).toBe(WIDTH_MAX);
    expect(clampWidth(300.4)).toBe(300);
  });

  it('falls back to the default for a width that is not a number', () => {
    expect(clampWidth(Number.NaN)).toBe(WIDTH_DEFAULT);
    expect(clampWidth(Number.POSITIVE_INFINITY)).toBe(WIDTH_DEFAULT);
  });
});

describe('remembered layout', () => {
  it('reads the defaults when nothing is stored', () => {
    expect(readPrefs()).toEqual(DEFAULT_PREFS);
  });

  it('round-trips the width, whether it is hidden, the view and each node’s expanded state', () => {
    const prefs = {
      width: 420,
      collapsed: true,
      expanded: { working: true, needs: false },
      view: 'projects' as const
    };
    writePrefs(prefs);
    expect(readPrefs()).toEqual(prefs);
  });

  it('shows the Sessions view when none, or an unknown one, is stored', () => {
    window.localStorage.setItem(PREFS_KEY, JSON.stringify({ width: 300 }));
    expect(readPrefs().view).toBe('sessions');
    window.localStorage.setItem(PREFS_KEY, JSON.stringify({ view: 'files' }));
    expect(readPrefs().view).toBe('sessions');
  });

  it('ignores junk and keeps the defaults for the parts it cannot read', () => {
    window.localStorage.setItem(
      PREFS_KEY,
      JSON.stringify({ width: 'wide', collapsed: 'yes', expanded: { a: 1, b: true } })
    );
    expect(readPrefs()).toEqual({
      width: WIDTH_DEFAULT,
      collapsed: false,
      expanded: { b: true },
      view: 'sessions'
    });
  });

  it('falls back to the defaults for text that is not JSON', () => {
    window.localStorage.setItem(PREFS_KEY, '{not json');
    expect(readPrefs()).toEqual(DEFAULT_PREFS);
  });

  it('still works when storage is blocked', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    expect(readPrefs()).toEqual(DEFAULT_PREFS);
    expect(() => writePrefs({ ...DEFAULT_PREFS, width: 300 })).not.toThrow();
  });

  it('uses the default expanded state for a node the owner has not touched', () => {
    expect(isExpanded(DEFAULT_PREFS, 'needs')).toBe(true);
    expect(isExpanded({ ...DEFAULT_PREFS, expanded: { needs: false } }, 'needs')).toBe(false);
  });
});
