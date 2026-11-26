import { useSyncExternalStore } from 'react';
import { z } from 'zod';

export const VIEWS = ['desktop', 'compact'] as const;
export type View = (typeof VIEWS)[number];

export const TABS = ['chat', 'explorer'] as const;
export type Tab = (typeof TABS)[number];

/**
 * Tab values from before the explorer, kept so old links still land: `sessions` and `projects` open
 * the spaces and agents, and `inbox` is sent to the answer view at `/inbox` by the root route.
 */
const LEGACY_TABS = ['sessions', 'inbox', 'projects'] as const;

/** Search params every route shares. Tab and view live in the URL so links can set them. */
export const shellSearchSchema = z.object({
  view: z.enum(VIEWS).optional().catch(undefined),
  tab: z
    .enum([...TABS, ...LEGACY_TABS])
    .optional()
    .catch(undefined)
});
export type ShellSearch = z.infer<typeof shellSearchSchema>;

/**
 * What a URL asks the phone to show: the page (`chat`), or the spaces and agents over it
 * (`explorer`). The old session, inbox and project tabs all show the spaces and agents.
 */
export function resolveTab(tab: ShellSearch['tab']): Tab {
  if (tab === undefined || tab === 'chat') return 'chat';
  return 'explorer';
}

/**
 * The desktop layout (activity bar, sidebar, page) needs a window at least 1024 px wide (Tailwind
 * `lg`), or one at least 640 px on both sides: a tablet, a small browser window, or iPhone Duo's
 * inner display (669 × 951 points, either way up). A phone is under 640 px one way or the other in
 * either orientation, as is iPhone Duo's outer display (466 × 678), so it keeps the compact layout.
 */
export const DESKTOP_WIDE = 1024;
export const DESKTOP_SIDE = 640;
export const DESKTOP_QUERY = `(min-width: ${DESKTOP_WIDE}px), (min-width: ${DESKTOP_SIDE}px) and (min-height: ${DESKTOP_SIDE}px)`;

/** What `DESKTOP_QUERY` matches, for a viewport of `width` × `height` CSS pixels. */
export function isDesktopViewport(width: number, height: number): boolean {
  return width >= DESKTOP_WIDE || (width >= DESKTOP_SIDE && height >= DESKTOP_SIDE);
}

/** `?view=` overrides the viewport; otherwise wide screens get the desktop shell. */
export function resolveView(requested: View | undefined, isWide: boolean): View {
  return requested ?? (isWide ? 'desktop' : 'compact');
}

function subscribe(onChange: () => void) {
  const query = window.matchMedia(DESKTOP_QUERY);
  query.addEventListener('change', onChange);
  return () => query.removeEventListener('change', onChange);
}

/** The layout in use: `?view=` if the URL sets it, else the viewport's. */
export function useShellView(requested: View | undefined): View {
  return resolveView(requested, useIsWide());
}

export function useIsWide(): boolean {
  return useSyncExternalStore(
    subscribe,
    () => window.matchMedia(DESKTOP_QUERY).matches,
    () => true
  );
}
