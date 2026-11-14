import { describe, expect, it } from 'vitest';
import { isApplePlatform, isShortcut, shortcutLabel } from './platform';

const key = (over: Partial<Parameters<typeof isShortcut>[0]> = {}) => ({
  key: 'b',
  metaKey: false,
  ctrlKey: false,
  altKey: false,
  shiftKey: false,
  ...over
});

describe('platform shortcuts', () => {
  it('uses ⌘ on Apple platforms and never Ctrl, which is the system’s text key there', () => {
    expect(isShortcut(key({ metaKey: true }), 'b', true)).toBe(true);
    // Ctrl+B moves back one character in a text field on macOS.
    expect(isShortcut(key({ ctrlKey: true }), 'b', true)).toBe(false);
    expect(isShortcut(key({ metaKey: true, ctrlKey: true }), 'b', true)).toBe(false);
  });

  it('uses Ctrl elsewhere and never the meta key', () => {
    expect(isShortcut(key({ ctrlKey: true }), 'b', false)).toBe(true);
    expect(isShortcut(key({ metaKey: true }), 'b', false)).toBe(false);
    expect(isShortcut(key({ metaKey: true, ctrlKey: true }), 'b', false)).toBe(false);
  });

  it('ignores other keys and extra modifiers, and matches either case', () => {
    expect(isShortcut(key({ key: 'B', metaKey: true }), 'b', true)).toBe(true);
    expect(isShortcut(key({ key: 'n', metaKey: true }), 'b', true)).toBe(false);
    expect(isShortcut(key({ metaKey: true, shiftKey: true }), 'b', true)).toBe(false);
    expect(isShortcut(key({ ctrlKey: true, altKey: true }), 'b', false)).toBe(false);
    expect(isShortcut(key(), 'b', true)).toBe(false);
  });

  it('writes the shortcut the way the platform does', () => {
    expect(shortcutLabel('b', true)).toBe('⌘B');
    expect(shortcutLabel('b', false)).toBe('Ctrl+B');
  });

  it('recognises Apple platforms', () => {
    expect(isApplePlatform({ platform: 'MacIntel', userAgent: '' })).toBe(true);
    expect(isApplePlatform({ platform: 'iPhone', userAgent: '' })).toBe(true);
    expect(
      isApplePlatform({
        platform: '',
        userAgent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)'
      })
    ).toBe(true);
    expect(isApplePlatform({ platform: 'Win32', userAgent: 'Mozilla/5.0 (Windows NT 10.0)' })).toBe(
      false
    );
    expect(
      isApplePlatform({ platform: 'Linux x86_64', userAgent: 'Mozilla/5.0 (X11; Linux x86_64)' })
    ).toBe(false);
  });
});
