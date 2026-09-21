import { writable } from 'svelte/store';

export type MobileTab = 'chat' | 'sessions' | 'tasks';

export const mobileTab = writable<MobileTab>('chat');
