/**
 * The prefix keys, as a terminal workspace manager has them: **Ctrl+B**, then one key. The prefix
 * arms the next key for a short while; that key moves between the current space's tabs, starts new
 * work, hides the sidebar, goes to the spaces or opens the menu. The rules are pure, so they test
 * without a DOM; the hook listens on the window, so the keys work while typing.
 */
import { useEffect, useRef, useState } from 'react';
import type { TabRow, Target } from '@/features/explorer';

/** How long the prefix waits for its key. After that the next key is typed as usual. */
export const PREFIX_TIMEOUT_MS = 2000;

/** What the status bar shows while the prefix waits: every key it takes. */
export const PREFIX_HINT =
  'ctrl+b · n next · p previous · 1–9 tab · c new · b sidebar · w spaces · g menu';

export type PrefixAction =
  | { do: 'open'; target: Target }
  | { do: 'new' }
  | { do: 'sidebar' }
  | { do: 'spaces' }
  | { do: 'menu' }
  /** A prefix key that has nowhere to go just now (the only tab, a tab number past the last). */
  | { do: 'nothing' };

type PrefixEvent = Pick<KeyboardEvent, 'key' | 'ctrlKey' | 'metaKey' | 'altKey' | 'shiftKey'>;

/** Whether `event` is the prefix itself: Ctrl+B and nothing else, on every platform. */
export function isPrefix(event: PrefixEvent): boolean {
  return (
    event.ctrlKey &&
    !event.metaKey &&
    !event.altKey &&
    !event.shiftKey &&
    event.key.toLowerCase() === 'b'
  );
}

/** The tab `step` places from the lit one, wrapping, among the tabs that lead somewhere. */
function stepTab(tabs: TabRow[], step: 1 | -1): PrefixAction {
  const at = tabs.findIndex((tab) => tab.current);
  for (let moved = 1; moved <= tabs.length; moved++) {
    const tab = tabs[(at + step * moved + tabs.length * moved) % tabs.length];
    if (tab && !tab.current && tab.target) return { do: 'open', target: tab.target };
  }
  return { do: 'nothing' };
}

/**
 * What the key after the prefix does, given the tabs on screen; null when it is not a prefix key,
 * so it is typed as usual.
 */
export function prefixAction(key: string, tabs: TabRow[]): PrefixAction | null {
  const lower = key.toLowerCase();
  if (lower === 'n') return stepTab(tabs, 1);
  if (lower === 'p') return stepTab(tabs, -1);
  if (/^[1-9]$/.test(key)) {
    const tab = tabs[Number(key) - 1];
    return tab?.target && !tab.current ? { do: 'open', target: tab.target } : { do: 'nothing' };
  }
  if (lower === 'c') return { do: 'new' };
  if (lower === 'b') return { do: 'sidebar' };
  if (lower === 'w') return { do: 'spaces' };
  if (lower === 'g') return { do: 'menu' };
  return null;
}

const MODIFIERS = new Set(['Shift', 'Control', 'Alt', 'Meta', 'CapsLock']);

/**
 * Listens for the prefix and hands the key after it to `onKey`, which says whether it took it. A
 * key it takes is not typed; any other key ends the wait and is typed as usual, and so does Escape.
 * Returns whether the prefix is waiting, so the shell can show the keys.
 */
export function usePrefixKeys(onKey: (key: string) => boolean): boolean {
  const [armed, setArmed] = useState(false);
  const handler = useRef(onKey);
  useEffect(() => {
    handler.current = onKey;
  }, [onKey]);

  useEffect(() => {
    let waiting = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const disarm = () => {
      waiting = false;
      clearTimeout(timer);
      setArmed(false);
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (isPrefix(event)) {
        // Taken on every platform, as in a terminal: on a Mac this is the text field's "back one
        // character", which the prefix replaces.
        event.preventDefault();
        waiting = true;
        setArmed(true);
        clearTimeout(timer);
        timer = setTimeout(disarm, PREFIX_TIMEOUT_MS);
        return;
      }
      if (!waiting || MODIFIERS.has(event.key)) return;
      disarm();
      if (event.key === 'Escape') {
        event.preventDefault();
        event.stopPropagation();
        return;
      }
      if (event.ctrlKey || event.metaKey || event.altKey) return;
      if (handler.current(event.key)) {
        event.preventDefault();
        event.stopPropagation();
      }
    };
    // Capture, so a key the prefix takes never reaches the composer.
    window.addEventListener('keydown', onKeyDown, true);
    return () => {
      window.removeEventListener('keydown', onKeyDown, true);
      clearTimeout(timer);
    };
  }, []);

  return armed;
}
