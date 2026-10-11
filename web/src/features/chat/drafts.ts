/**
 * Unsent text, one draft per conversation. It lives in sessionStorage so it survives route
 * changes and reloads of the tab but not a closed tab. Storage can be missing or full (private
 * windows, blocked site data), so every access is guarded and the composer works without it.
 */
const PREFIX = 'mainloop:draft:';

/** The key before the first message exists: there is no conversation id yet. */
export const NEW_CONVERSATION = 'new';

export function loadDraft(key: string): string {
  try {
    return sessionStorage.getItem(PREFIX + key) ?? '';
  } catch {
    return '';
  }
}

/** An empty draft removes the entry. */
export function saveDraft(key: string, text: string): void {
  try {
    if (text) sessionStorage.setItem(PREFIX + key, text);
    else sessionStorage.removeItem(PREFIX + key);
  } catch {
    /* The draft just does not survive a reload. */
  }
}

export function clearDraft(key: string): void {
  saveDraft(key, '');
}
