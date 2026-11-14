import { useSyncExternalStore } from 'react';
import { z } from 'zod';

export const VIEWS = ['desktop', 'compact'] as const;
export type View = (typeof VIEWS)[number];

export const TABS = ['chat', 'explorer'] as const;
export type Tab = (typeof TABS)[number];

/**
 * Tab values from before the explorer, kept so old links still land: `sessions` and `projects` open
 * the Explorer tab, and `inbox` is sent to the answer view at `/inbox` by the root route.
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

/** The phone tab a URL asks for. The old session, inbox and project tabs all show the explorer. */
export function resolveTab(tab: ShellSearch['tab']): Tab {
  if (tab === undefined || tab === 'chat') return 'chat';
  return 'explorer';
}

/** The three items of the phone's tab bar. `needs` is the answer view at `/inbox`, not a `?tab=`. */
export type NavTab = 'chat' | 'needs' | 'explorer';

/**
 * Which tab-bar item is lit. It follows where the owner is: the tree, the main chat, the answer
 * view, or a page reached from the tree (a task, a session, a project, health), which lights the
 * tree's own tab, since that is where the page was opened from and where "back" leads.
 */
export function activeNavTab(tab: Tab, pathname: string): NavTab {
  if (tab === 'explorer') return 'explorer';
  const path = pathname.replace(/\/+$/, '') || '/';
  if (path === '/') return 'chat';
  if (path === '/inbox') return 'needs';
  return 'explorer';
}

/** Desktop layout starts here (Tailwind `lg`), as the frozen app did. */
export const DESKTOP_QUERY = '(min-width: 1024px)';

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
