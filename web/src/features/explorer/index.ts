/** Public surface of the explorer: the desktop sidebar, the phone pane, the chat starters, the data and prefs the shell shares, and the Needs-you count. */
export { ChatStarters } from './ChatStarters';
export { ExplorerPane, ExplorerSidebar, useExplorerPrefs, useNeedsCount } from './ExplorerPanel';
export { needsCount, shellCounts, SIDEBAR_VIEWS, type SidebarView } from './model';
export type { ExplorerPrefs } from './prefs';
export { useExplorer } from './useExplorer';
