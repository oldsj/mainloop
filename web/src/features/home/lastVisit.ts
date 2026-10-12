import { useEffect, useState } from 'react';

/**
 * The owner's last visit, kept per browser. It is a per-viewer convenience only: if storage is
 * blocked or empty, home still works and the strip and "finished" rows simply have no baseline.
 * The contract has no per-owner read cursor for tasks, so this is the only source for "since".
 */
const KEY = 'mainloop.home.lastVisit';

/** The previous visit's time in ms, or null when none is held or storage cannot be read. */
export function readLastVisit(): number | null {
  try {
    const raw = window.localStorage.getItem(KEY);
    const value = raw === null ? NaN : Number(raw);
    return Number.isFinite(value) ? value : null;
  } catch {
    return null;
  }
}

export function writeLastVisit(at: number = Date.now()): void {
  try {
    window.localStorage.setItem(KEY, String(at));
  } catch {
    // Storage is blocked; the next visit simply has no baseline.
  }
}

/**
 * The previous visit, read once when the app shell mounts, with this visit written after. The shell
 * calls it once and passes the value down, so the desktop aside and the phone tabs compare against
 * the same baseline even though they mount at different times.
 */
export function useLastVisit(): number | null {
  const [previous] = useState(readLastVisit);
  useEffect(() => writeLastVisit(), []);
  return previous;
}
