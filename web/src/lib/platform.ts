/**
 * Which modifier a keyboard shortcut uses. On Apple platforms it is ⌘ and only ⌘: Ctrl there
 * belongs to the system's text keys (Ctrl+B moves back one character in any text field), so a
 * shortcut must never take it. Everywhere else it is Ctrl and only Ctrl.
 */
export function isApplePlatform(
  nav: Pick<Navigator, 'platform' | 'userAgent'> = navigator
): boolean {
  const platform =
    (nav as { userAgentData?: { platform?: string } }).userAgentData?.platform ??
    nav.platform ??
    '';
  return /mac|iphone|ipad|ipod/i.test(platform) || /Mac OS X|iPhone|iPad/.test(nav.userAgent ?? '');
}

type ModifierKeys = Pick<KeyboardEvent, 'key' | 'metaKey' | 'ctrlKey' | 'altKey' | 'shiftKey'>;

/** Whether `event` is the platform's shortcut for `key`: ⌘+key on Apple, Ctrl+key elsewhere. */
export function isShortcut(event: ModifierKeys, key: string, apple = isApplePlatform()): boolean {
  if (event.key.toLowerCase() !== key.toLowerCase()) return false;
  if (event.altKey || event.shiftKey) return false;
  return apple ? event.metaKey && !event.ctrlKey : event.ctrlKey && !event.metaKey;
}

/** How the shortcut for `key` is written on this platform: "⌘B" or "Ctrl+B". */
export function shortcutLabel(key: string, apple = isApplePlatform()): string {
  return apple ? `⌘${key.toUpperCase()}` : `Ctrl+${key.toUpperCase()}`;
}
