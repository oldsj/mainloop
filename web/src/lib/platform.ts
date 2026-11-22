/**
 * Which modifier a keyboard shortcut uses. On Apple platforms it is ⌘ and only ⌘: Ctrl there
 * belongs to the system's text keys (Ctrl+K deletes to the end of the line in any text field), so
 * a shortcut does not take it. Everywhere else it is Ctrl and only Ctrl. The one exception is the
 * prefix, Ctrl+B on every platform (`shell/prefixKeys`), which is a terminal's key, not a shortcut.
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

/**
 * How the sidebar's shortcut is written: ⌘B on Apple platforms. Elsewhere Ctrl+B is the prefix key
 * (see `shell/prefixKeys`), so the sidebar is the prefix and then B.
 */
export function sidebarShortcutLabel(apple = isApplePlatform()): string {
  return apple ? '⌘B' : 'Ctrl+B B';
}
