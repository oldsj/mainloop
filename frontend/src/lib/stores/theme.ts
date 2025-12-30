/**
 * Theme state management with localStorage persistence
 */

import { writable, derived } from 'svelte/store';
import { browser } from '$app/environment';

export type ThemeName = 'eighties' | 'dracula' | 'nord' | 'gruvbox' | 'tokyo-night';

interface ThemeState {
  current: ThemeName;
}

const STORAGE_KEY = 'mainloop-theme';
const DEFAULT_THEME: ThemeName = 'eighties';

function isValidTheme(value: string): value is ThemeName {
  return ['eighties', 'dracula', 'nord', 'gruvbox', 'tokyo-night'].includes(value);
}

function getInitialTheme(): ThemeName {
  if (!browser) return DEFAULT_THEME;
  const stored = localStorage.getItem(STORAGE_KEY);
  if (stored && isValidTheme(stored)) {
    return stored as ThemeName;
  }
  return DEFAULT_THEME;
}

function createThemeStore() {
  const { subscribe, set } = writable<ThemeState>({
    current: getInitialTheme()
  });

  return {
    subscribe,

    setTheme(theme: ThemeName) {
      if (browser) {
        localStorage.setItem(STORAGE_KEY, theme);
        document.documentElement.className = `theme-${theme}`;
      }
      set({ current: theme });
    },

    initialize() {
      if (browser) {
        const theme = getInitialTheme();
        document.documentElement.className = `theme-${theme}`;
      }
    }
  };
}

export const themeStore = createThemeStore();
export const currentTheme = derived(themeStore, ($store) => $store.current);

export const themes: { id: ThemeName; name: string; accent: string }[] = [
  { id: 'eighties', name: 'Eighties', accent: '#6699cc' },
  { id: 'dracula', name: 'Dracula', accent: '#bd93f9' },
  { id: 'nord', name: 'Nord', accent: '#88c0d0' },
  { id: 'gruvbox', name: 'Gruvbox', accent: '#fabd2f' },
  { id: 'tokyo-night', name: 'Tokyo Night', accent: '#7aa2f7' }
];
