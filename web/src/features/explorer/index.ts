/** Public surface of the explorer: the desktop sidebar, the phone pane, the chat starters, the data and prefs the shell shares, and the Needs-you count. */
export { ChatStarters } from './ChatStarters';
export { ExplorerPane, ExplorerSidebar, useExplorerPrefs, useNeedsCount } from './ExplorerPanel';
export {
  needsCount,
  shellCounts,
  SIDEBAR_VIEWS,
  type ExplorerNode,
  type SidebarView,
  type Target
} from './model';
export type { ExplorerPrefs } from './prefs';
export { useAsk, useExplorer, useOpenTarget } from './useExplorer';
