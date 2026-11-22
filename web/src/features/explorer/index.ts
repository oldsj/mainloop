/** Public surface of the explorer: the desktop sidebar, the phone pane, the chat starters, the data and prefs the shell shares, and the Needs-you count. */
export { ChatStarters } from './ChatStarters';
export { ExplorerPane, ExplorerSidebar, useExplorerPrefs, useNeedsCount } from './ExplorerPanel';
export { needsCount, shellCounts, type ExplorerNode, type Target } from './model';
export type { ExplorerPrefs } from './prefs';
export { newTaskPrefill, workingCount } from './agents';
export { TabStrip } from './TabStrip';
export { useAgentColumn, useAsk, useExplorer, useOpenTarget } from './useExplorer';
