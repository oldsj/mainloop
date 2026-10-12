import { describe, expect, it } from 'vitest';
import { resolveView, shellSearchSchema } from './view';

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
