import { readable } from 'svelte/store';
import { browser } from '$app/environment';

const MOBILE_BREAKPOINT = 768; // md breakpoint in Tailwind

function createViewportStore() {
  return readable(browser ? window.innerWidth < MOBILE_BREAKPOINT : false, (set) => {
    if (!browser) return;

    const query = window.matchMedia(`(max-width: ${MOBILE_BREAKPOINT - 1}px)`);
    set(query.matches);

    const handler = (e: MediaQueryListEvent) => set(e.matches);
    query.addEventListener('change', handler);

    return () => query.removeEventListener('change', handler);
  });
}

export const isMobile = createViewportStore();
