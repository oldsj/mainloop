/**
 * The owner's last visit to home, kept per browser. It is a per-viewer convenience only: if storage
 * is blocked or empty, home still works and the strip and "finished" rows simply have no baseline.
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
