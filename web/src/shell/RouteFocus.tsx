import { useRouterState } from '@tanstack/react-router';
import { useEffect, useRef } from 'react';

/** The main landmark both shells render. A route change moves focus here. */
export const MAIN_ID = 'main-content';

/**
 * After a route change, focus moves to the main landmark, so a keyboard or screen-reader user starts
 * the new page instead of the body. The page the app opens on is left alone.
 */
export function RouteFocus() {
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const shown = useRef(pathname);
  useEffect(() => {
    if (shown.current === pathname) return;
    shown.current = pathname;
    document.getElementById(MAIN_ID)?.focus();
  }, [pathname]);
  return null;
}
