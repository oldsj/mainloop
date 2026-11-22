import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { TabRow } from '@/features/explorer';
import { isPrefix, PREFIX_TIMEOUT_MS, prefixAction, usePrefixKeys } from './prefixKeys';

const tab = (id: string, current = false): TabRow => ({
  id,
  label: id,
  target: { to: 'task', id },
  current
});

/** Three agents' tabs with the second lit, then the page's own tab, which leads nowhere. */
const TABS: TabRow[] = [
  tab('a'),
  tab('b', true),
  tab('c'),
  { id: 'page', label: 'inbox', current: false }
];

const key = (name: string, modifiers: Partial<KeyboardEventInit> = {}) =>
  new KeyboardEvent('keydown', { key: name, bubbles: true, cancelable: true, ...modifiers });

describe('the prefix', () => {
  it('is Ctrl+B alone, on every platform', () => {
    expect(isPrefix(key('b', { ctrlKey: true }))).toBe(true);
    expect(isPrefix(key('B', { ctrlKey: true }))).toBe(true);
    expect(isPrefix(key('b', { metaKey: true }))).toBe(false);
    expect(isPrefix(key('b', { ctrlKey: true, shiftKey: true }))).toBe(false);
    expect(isPrefix(key('b', { ctrlKey: true, altKey: true }))).toBe(false);
    expect(isPrefix(key('b'))).toBe(false);
  });
});

describe('the key after the prefix', () => {
  it('moves to the next and the previous tab, wrapping, past a tab that leads nowhere', () => {
    expect(prefixAction('n', TABS)).toEqual({ do: 'open', target: { to: 'task', id: 'c' } });
    expect(prefixAction('p', TABS)).toEqual({ do: 'open', target: { to: 'task', id: 'a' } });
    // From the last agent, next wraps to the first: the page's own tab is not a place to go.
    const onLast = [tab('a'), tab('b'), tab('c', true), TABS[3]!];
    expect(prefixAction('n', onLast)).toEqual({ do: 'open', target: { to: 'task', id: 'a' } });
    const onFirst = [tab('a', true), tab('b'), tab('c')];
    expect(prefixAction('p', onFirst)).toEqual({ do: 'open', target: { to: 'task', id: 'c' } });
  });

  it('goes to a tab by its number', () => {
    expect(prefixAction('1', TABS)).toEqual({ do: 'open', target: { to: 'task', id: 'a' } });
    expect(prefixAction('3', TABS)).toEqual({ do: 'open', target: { to: 'task', id: 'c' } });
  });

  it('takes the key but goes nowhere when there is nowhere to go', () => {
    // The lit tab, a tab that leads nowhere, a number past the last, and the only tab there is
    expect(prefixAction('2', TABS)).toEqual({ do: 'nothing' });
    expect(prefixAction('4', TABS)).toEqual({ do: 'nothing' });
    expect(prefixAction('9', TABS)).toEqual({ do: 'nothing' });
    expect(prefixAction('n', [tab('only', true)])).toEqual({ do: 'nothing' });
  });

  it('starts new work, hides the sidebar, goes to the spaces and opens the menu', () => {
    expect(prefixAction('c', TABS)).toEqual({ do: 'new' });
    expect(prefixAction('b', TABS)).toEqual({ do: 'sidebar' });
    expect(prefixAction('w', TABS)).toEqual({ do: 'spaces' });
    expect(prefixAction('g', TABS)).toEqual({ do: 'menu' });
    expect(prefixAction('C', TABS)).toEqual({ do: 'new' });
  });

  it('is not a prefix key otherwise, so it is typed as usual', () => {
    expect(prefixAction('x', TABS)).toBeNull();
    expect(prefixAction('0', TABS)).toBeNull();
    expect(prefixAction('Enter', TABS)).toBeNull();
  });
});

describe('usePrefixKeys', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  const press = (event: KeyboardEvent, target: EventTarget = document.body) => {
    act(() => {
      target.dispatchEvent(event);
    });
    return event;
  };

  it('arms on the prefix and hands the next key over, which is then not typed', () => {
    const onKey = vi.fn(() => true);
    const { result } = renderHook(() => usePrefixKeys(onKey));
    expect(result.current).toBe(false);

    // When the owner presses Ctrl+B, then n
    expect(press(key('b', { ctrlKey: true })).defaultPrevented).toBe(true);
    expect(result.current).toBe(true);
    const next = press(key('n'));

    // Then the key went to the handler and nowhere else, and the wait is over
    expect(onKey).toHaveBeenCalledWith('n');
    expect(next.defaultPrevented).toBe(true);
    expect(result.current).toBe(false);
    // And a second n is an ordinary key again
    expect(press(key('n')).defaultPrevented).toBe(false);
    expect(onKey).toHaveBeenCalledTimes(1);
  });

  it('takes the key before a text field sees it', () => {
    const onKey = vi.fn(() => true);
    renderHook(() => usePrefixKeys(onKey));
    const field = document.createElement('textarea');
    document.body.append(field);
    const seen = vi.fn();
    field.addEventListener('keydown', seen);

    press(key('b', { ctrlKey: true }), field);
    press(key('n'), field);

    // The field heard the prefix (already cancelled) but never the n
    expect(seen).toHaveBeenCalledTimes(1);
    field.remove();
  });

  it('lets a key through that is not one of its own, and ends the wait', () => {
    const onKey = vi.fn(() => false);
    const { result } = renderHook(() => usePrefixKeys(onKey));

    press(key('b', { ctrlKey: true }));
    const other = press(key('x'));

    expect(onKey).toHaveBeenCalledWith('x');
    expect(other.defaultPrevented).toBe(false);
    expect(result.current).toBe(false);
  });

  it('waits through a modifier on its own, and gives up on Escape or after a moment', () => {
    const onKey = vi.fn(() => true);
    const { result } = renderHook(() => usePrefixKeys(onKey));

    // Shift on its way to a capital is not the key
    press(key('b', { ctrlKey: true }));
    press(key('Shift', { shiftKey: true }));
    expect(result.current).toBe(true);

    // Escape ends the wait without doing anything
    expect(press(key('Escape')).defaultPrevented).toBe(true);
    expect(result.current).toBe(false);
    expect(onKey).not.toHaveBeenCalled();

    // And so does time: the n typed later is just an n
    press(key('b', { ctrlKey: true }));
    act(() => {
      vi.advanceTimersByTime(PREFIX_TIMEOUT_MS);
    });
    expect(result.current).toBe(false);
    expect(press(key('n')).defaultPrevented).toBe(false);
    expect(onKey).not.toHaveBeenCalled();
  });

  it('leaves a browser shortcut alone while it waits', () => {
    const onKey = vi.fn(() => true);
    renderHook(() => usePrefixKeys(onKey));

    press(key('b', { ctrlKey: true }));
    const reload = press(key('n', { metaKey: true }));

    expect(reload.defaultPrevented).toBe(false);
    expect(onKey).not.toHaveBeenCalled();
  });
});
