import { describe, expect, it } from 'vitest';
import { activeNavTab, resolveView, shellSearchSchema } from './view';

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

describe('shellSearchSchema', () => {
  it('drops unknown view and tab values instead of failing the route', () => {
    expect(shellSearchSchema.parse({ view: 'tablet', tab: 'inbox' })).toEqual({
      view: undefined,
      tab: 'inbox'
    });
  });
});

describe('activeNavTab', () => {
  it('lights the tree whenever it is showing', () => {
    expect(activeNavTab('explorer', '/')).toBe('explorer');
    expect(activeNavTab('explorer', '/inbox')).toBe('explorer');
  });

  it('follows the page otherwise', () => {
    expect(activeNavTab('chat', '/')).toBe('chat');
    expect(activeNavTab('chat', '/inbox')).toBe('needs');
    expect(activeNavTab('chat', '/inbox/')).toBe('needs');
    for (const path of ['/health', '/tasks/t', '/sessions/s', '/projects/p', '/schedules', '/nope'])
      expect(activeNavTab('chat', path)).toBe('explorer');
  });
});
