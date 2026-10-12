/**
 * The explorer's remembered layout, per browser: the panel width, whether it is folded to a rail,
 * and each node's expanded state. Storage can be blocked or hold junk, so every read and write is
 * guarded and a bad value falls back to the default.
 */
import { DEFAULT_EXPANDED } from './model';

export const PREFS_KEY = 'mainloop.explorer.v1';
export const WIDTH_MIN = 240;
export const WIDTH_MAX = 480;
export const WIDTH_DEFAULT = 384;
/** The rail's width when the panel is folded. */
export const RAIL_WIDTH = 40;

export interface ExplorerPrefs {
  width: number;
  collapsed: boolean;
  /** Node id to its expanded state. A node not listed takes its default. */
  expanded: Record<string, boolean>;
}

export const DEFAULT_PREFS: ExplorerPrefs = {
  width: WIDTH_DEFAULT,
  collapsed: false,
  expanded: {}
};

/** Keeps a dragged width inside the range the panel can show. */
export function clampWidth(width: number): number {
  if (!Number.isFinite(width)) return WIDTH_DEFAULT;
  return Math.min(WIDTH_MAX, Math.max(WIDTH_MIN, Math.round(width)));
}

export function isExpanded(prefs: ExplorerPrefs, id: string): boolean {
  return prefs.expanded[id] ?? DEFAULT_EXPANDED.has(id);
}

function parse(raw: string | null): ExplorerPrefs {
  if (raw === null) return DEFAULT_PREFS;
  try {
    const value: unknown = JSON.parse(raw);
    if (typeof value !== 'object' || value === null) return DEFAULT_PREFS;
    const record = value as Record<string, unknown>;
    const expanded: Record<string, boolean> = {};
    if (typeof record.expanded === 'object' && record.expanded !== null) {
      for (const [id, open] of Object.entries(record.expanded)) {
        if (typeof open === 'boolean') expanded[id] = open;
      }
    }
    return {
      width: typeof record.width === 'number' ? clampWidth(record.width) : WIDTH_DEFAULT,
      collapsed: record.collapsed === true,
      expanded
    };
  } catch {
    return DEFAULT_PREFS;
  }
}

export function readPrefs(): ExplorerPrefs {
  try {
    return parse(window.localStorage.getItem(PREFS_KEY));
  } catch {
    return DEFAULT_PREFS;
  }
}

export function writePrefs(prefs: ExplorerPrefs): void {
  try {
    window.localStorage.setItem(PREFS_KEY, JSON.stringify(prefs));
  } catch {
    // Storage is blocked: the layout still works for this visit, it just is not remembered.
  }
}
