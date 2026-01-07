import { writable } from 'svelte/store';

/**
 * Store for preserving draft message across tab switches on mobile
 */
export const draftMessage = writable<string>('');
