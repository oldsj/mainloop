import { useCallback, useSyncExternalStore } from 'react';

/** Keep in sync with public/theme-init.js, which applies the saved theme before first paint. */
export const THEMES = [
  { id: 'eighties', label: 'eighties' },
  { id: 'dracula', label: 'dracula' },
  { id: 'nord', label: 'nord' },
  { id: 'gruvbox', label: 'gruvbox' },
  { id: 'tokyo-night', label: 'tokyo-night' }
] as const;

export type ThemeId = (typeof THEMES)[number]['id'];

export const DEFAULT_THEME: ThemeId = 'eighties';
export const THEME_STORAGE_KEY = 'mainloop-theme';

export const isThemeId = (value: unknown): value is ThemeId =>
  THEMES.some((theme) => theme.id === value);

/** The theme on `<html>`: no attribute means the default (the :root block of theme.css). */
export function currentTheme(root: HTMLElement = document.documentElement): ThemeId {
  const value = root.getAttribute('data-theme');
  return isThemeId(value) ? value : DEFAULT_THEME;
}

export function applyTheme(theme: ThemeId, root: HTMLElement = document.documentElement): void {
  if (theme === DEFAULT_THEME) root.removeAttribute('data-theme');
  else root.setAttribute('data-theme', theme);
  try {
    localStorage.setItem(THEME_STORAGE_KEY, theme);
  } catch {
    // Storage can be blocked; the theme still applies for this page load.
  }
}

/** Reads the theme from `<html>` (set before paint) and re-renders when it changes. */
export function useTheme(): [ThemeId, (theme: ThemeId) => void] {
  const subscribe = useCallback((onChange: () => void) => {
    const observer = new MutationObserver(onChange);
    observer.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ['data-theme']
    });
    return () => observer.disconnect();
  }, []);
  const theme = useSyncExternalStore(
    subscribe,
    () => currentTheme(),
    () => DEFAULT_THEME
  );
  return [theme, applyTheme];
}
