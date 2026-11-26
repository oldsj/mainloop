import { describe, expect, it } from 'vitest';
import {
  DESKTOP_QUERY,
  isDesktopViewport,
  resolveTab,
  resolveView,
  shellSearchSchema
} from './view';

describe('resolveView', () => {
  it('follows the viewport when no view is requested', () => {
    expect(resolveView(undefined, true)).toBe('desktop');
    expect(resolveView(undefined, false)).toBe('compact');
  });

  it('lets ?view= override the viewport', () => {
    expect(resolveView('compact', true)).toBe('compact');
    expect(resolveView('desktop', false)).toBe('desktop');
  });
});

describe('isDesktopViewport', () => {
  it.each([
    ['a laptop', 1280, 800, true],
    ['a short wide window', 1100, 500, true],
    ['iPhone Duo open, upright', 669, 951, true],
    ['iPhone Duo open, on its side', 951, 669, true],
    ['an iPad mini, upright', 744, 1133, true],
    ['iPhone Duo closed, upright', 466, 678, false],
    ['iPhone Duo closed, on its side', 678, 466, false],
    ['a large phone, upright', 440, 956, false],
    ['a large phone, on its side', 956, 440, false]
  ])('%s (%i × %i): desktop %s', (_name, width, height, desktop) => {
    expect(isDesktopViewport(width, height)).toBe(desktop);
  });

  it('is the media query the shell listens to', () => {
    expect(DESKTOP_QUERY).toBe('(min-width: 1024px), (min-width: 640px) and (min-height: 640px)');
  });
});

describe('shellSearchSchema', () => {
  it('drops unknown view and tab values instead of failing the route', () => {
    expect(shellSearchSchema.parse({ view: 'tablet', tab: 'inbox' })).toEqual({
      view: undefined,
      tab: 'inbox'
    });
  });
});

describe('resolveTab', () => {
  it('shows the page unless the URL asks for the spaces and agents', () => {
    expect(resolveTab(undefined)).toBe('chat');
    expect(resolveTab('chat')).toBe('chat');
    expect(resolveTab('explorer')).toBe('explorer');
  });

  it('maps the tabs from before the explorer to the spaces and agents', () => {
    expect(resolveTab('sessions')).toBe('explorer');
    expect(resolveTab('projects')).toBe('explorer');
  });
});
