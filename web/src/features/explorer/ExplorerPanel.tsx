import { PanelLeftClose } from 'lucide-react';
import { useCallback, useRef, useState, type KeyboardEvent, type PointerEvent } from 'react';
import { Button } from '@/components/ui/button';
import { sidebarShortcutLabel } from '@/lib/platform';
import {
  WIDTH_MAX,
  WIDTH_MIN,
  clampWidth,
  readPrefs,
  writePrefs,
  type ExplorerPrefs
} from './prefs';
import { AgentColumn } from './AgentColumn';
import { useAsk, useOpenTarget, type AgentColumnState } from './useExplorer';

/** Arrow-key step on the resize handle, in pixels. */
const RESIZE_STEP = 16;

/** The prefs, read once per mount and written on every change, so a reload keeps the layout. */
export function useExplorerPrefs() {
  const [prefs, setPrefs] = useState<ExplorerPrefs>(readPrefs);
  const update = useCallback((change: (prev: ExplorerPrefs) => ExplorerPrefs) => {
    setPrefs((prev) => {
      const next = change(prev);
      writePrefs(next);
      return next;
    });
  }, []);
  return { prefs, update };
}

/** Desktop only: folds the sidebar away; the activity bar's button or the shortcut brings it back. */
function HideSidebar({ onClick }: { onClick: () => void }) {
  return (
    <Button
      type="button"
      variant="ghost"
      size="sm"
      aria-label="Hide sidebar"
      title={`Hide sidebar (${sidebarShortcutLabel()})`}
      onClick={onClick}
    >
      <PanelLeftClose aria-hidden="true" />
    </Button>
  );
}

export interface ExplorerSidebarProps {
  prefs: ExplorerPrefs;
  update: ReturnType<typeof useExplorerPrefs>['update'];
  /** The spaces and agents. The shell owns them: the tab strip and the status bar read them too. */
  column: AgentColumnState;
  ready: boolean;
  failed: boolean;
  /** Opens the command palette: **menu**. */
  onMenu: () => void;
}

/**
 * Desktop: the sidebar, between the activity bar and the page. It is the spaces and their agents,
 * with **new** and **menu** between the two lists. It is resizable from its right edge, and is
 * hidden with its own button, the activity bar's, or the sidebar shortcut (⌘B on a Mac; the prefix
 * and B anywhere); the activity bar brings it back. The shell owns the prefs and the data, since the tab strip and the status bar read them
 * too.
 */
export function ExplorerSidebar({
  prefs,
  update,
  column,
  ready,
  failed,
  onMenu
}: ExplorerSidebarProps) {
  const open = useOpenTarget();
  const ask = useAsk();
  const drag = useRef<{ startX: number; startWidth: number; width: number } | null>(null);
  // The width while a drag is in progress. It is drawn from here and saved only when the drag ends.
  const [dragWidth, setDragWidth] = useState<number | null>(null);

  const collapse = () => update((prev) => ({ ...prev, collapsed: true }));

  const setWidth = (width: number) => update((prev) => ({ ...prev, width: clampWidth(width) }));

  const onPointerDown = (event: PointerEvent<HTMLDivElement>) => {
    event.currentTarget.setPointerCapture(event.pointerId);
    drag.current = { startX: event.clientX, startWidth: prefs.width, width: prefs.width };
  };
  // The sidebar sits on the left, so dragging its right edge to the right widens it.
  const onPointerMove = (event: PointerEvent<HTMLDivElement>) => {
    const start = drag.current;
    if (!start) return;
    start.width = clampWidth(start.startWidth + (event.clientX - start.startX));
    // Each move is a render only; the width is written once, when the drag ends.
    setDragWidth(start.width);
  };
  const onPointerUp = () => {
    const start = drag.current;
    drag.current = null;
    setDragWidth(null);
    if (start && start.width !== prefs.width) setWidth(start.width);
  };
  const onHandleKey = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === 'ArrowRight') setWidth(prefs.width + RESIZE_STEP);
    else if (event.key === 'ArrowLeft') setWidth(prefs.width - RESIZE_STEP);
    else return;
    event.preventDefault();
  };

  return (
    <aside
      aria-label="Sidebar"
      data-testid="explorer-aside"
      style={{ width: dragWidth ?? prefs.width }}
      className="border-border-subtle relative flex min-h-0 shrink-0 flex-col border-r"
    >
      <AgentColumn
        model={column.model}
        page={column.page}
        space={column.space}
        ready={ready}
        failed={failed}
        onOpen={open}
        onAsk={ask}
        onMenu={onMenu}
        action={<HideSidebar onClick={collapse} />}
      />
      {/* A focusable window splitter (WAI-ARIA): the separator takes keys, so it is not a plain static element. */}
      {/* eslint-disable jsx-a11y/no-noninteractive-element-interactions, jsx-a11y/no-noninteractive-tabindex */}
      <div
        role="separator"
        aria-orientation="vertical"
        aria-label="Resize sidebar"
        aria-valuemin={WIDTH_MIN}
        aria-valuemax={WIDTH_MAX}
        aria-valuenow={prefs.width}
        tabIndex={0}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
        onKeyDown={onHandleKey}
        className="hover:bg-accent focus-visible:bg-ring absolute inset-y-0 right-0 z-10 w-1 cursor-col-resize touch-none outline-none focus-visible:w-1.5"
      />
      {/* eslint-enable jsx-a11y/no-noninteractive-element-interactions, jsx-a11y/no-noninteractive-tabindex */}
    </aside>
  );
}
