/**
 * Public surface of the explorer: the desktop sidebar, the agent column, the tab strip, the chat
 * starters, the rules for one row, and the data, counts and prefs the shell shares.
 */
export { ChatStarters } from './ChatStarters';
export { AgentColumn } from './AgentColumn';
export { ExplorerSidebar, useExplorerPrefs } from './ExplorerPanel';
export {
  askPrefill,
  sessionNode,
  shortId,
  stagedTaskNode,
  workspaceNode,
  type ExplorerSource,
  type Target
} from './model';
export { loadingTabs, newTaskPrefill, spaceTabs, workingCount, type TabRow } from './agents';
export { TabStrip } from './TabStrip';
export {
  useAgentColumn,
  useAsk,
  useExplorer,
  useExplorerSource,
  useOpenTarget
} from './useExplorer';
