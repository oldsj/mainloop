import { useSyncExternalStore } from 'react';
import { z } from 'zod';

export const VIEWS = ['desktop', 'compact'] as const;
export type View = (typeof VIEWS)[number];

export const TABS = ['chat', 'sessions', 'inbox', 'projects'] as const;
export type Tab = (typeof TABS)[number];

/** Search params every route shares. Tab and view live in the URL so links can set them. */
export const shellSearchSchema = z.object({
  view: z.enum(VIEWS).optional().catch(undefined),
  tab: z.enum(TABS).optional().catch(undefined)
});
export type ShellSearch = z.infer<typeof shellSearchSchema>;

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

export function useIsWide(): boolean {
  return useSyncExternalStore(
    subscribe,
    () => window.matchMedia(DESKTOP_QUERY).matches,
    () => true
  );
}
