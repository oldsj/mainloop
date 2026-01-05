import { writable } from 'svelte/store';

export type MobileTab = 'chat' | 'tasks';

export const mobileTab = writable<MobileTab>('chat');
