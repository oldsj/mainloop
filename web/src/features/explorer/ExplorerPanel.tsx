import { useLocation } from '@tanstack/react-router';
import { PanelLeftClose } from 'lucide-react';
import {
  useCallback,
  useRef,
  useState,
  type KeyboardEvent,
  type PointerEvent,
  type ReactNode
} from 'react';
import { Button } from '@/components/ui/button';
import { shortcutLabel } from '@/lib/platform';
import {
  needsCount,
  SIDEBAR_VIEWS,
  targetOfLocation,
  viewNodes,
  type ExplorerNode,
  type SidebarView
} from './model';
import {
  WIDTH_MAX,
  WIDTH_MIN,
  clampWidth,
  isExpanded,
  readPrefs,
  writePrefs,
  type ExplorerPrefs
} from './prefs';
import { ExplorerTree } from './ExplorerTree';
import { useAsk, useExplorer, useOpenTarget } from './useExplorer';

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

function useTree(update: ReturnType<typeof useExplorerPrefs>['update']) {
  const open = useOpenTarget();
  const ask = useAsk();
  const toggle = useCallback(
    (id: string) =>
      update((prev) => ({
        ...prev,
        expanded: { ...prev.expanded, [id]: !isExpanded(prev, id) }
      })),
    [update]
  );
  return { open, ask, toggle };
}

interface BodyProps {
  nodes: ExplorerNode[];
  ready: boolean;
  failed: boolean;
  prefs: ExplorerPrefs;
  onToggle: (id: string) => void;
  onOpen: ReturnType<typeof useOpenTarget>;
  onAsk: (text: string) => void;
  /** The panel's heading: the view's name on desktop, "Explorer" on the phone. */
  title: string;
  /** Desktop only: folds the sidebar away; the activity bar brings it back. */
  onCollapse?: () => void;
  /** Phone only: controls at the end of the header (health and the theme). */
  actions?: ReactNode;
}

/** The header and the tree. Shared by the desktop sidebar and the phone pane. */
function ExplorerBody({
  nodes,
  ready,
  failed,
  prefs,
  onToggle,
  onOpen,
  onAsk,
  title,
  onCollapse,
  actions
}: BodyProps) {
  // The row for the page being shown is marked, so the tree says where the owner is.
  const current = useLocation({
    select: (location) => targetOfLocation(location.pathname, location.search),
    structuralSharing: true
  });
  return (
    <section aria-labelledby="explorer-heading" className="flex min-h-0 flex-1 flex-col">
      <header className="gap-sm px-md py-xs border-border-subtle flex items-center justify-between border-b">
        <h2
          id="explorer-heading"
          className="text-muted-foreground text-2xs font-medium tracking-wide uppercase"
        >
          {title}
        </h2>
        {onCollapse ? (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            aria-label="Hide sidebar"
            title={`Hide sidebar (${shortcutLabel('b')})`}
            onClick={onCollapse}
          >
            <PanelLeftClose aria-hidden="true" />
          </Button>
        ) : null}
        {actions ? <div className="gap-xs flex items-center">{actions}</div> : null}
      </header>
      <div className="p-xs min-h-0 flex-1 overflow-y-auto">
        {failed ? (
          <p role="alert" className="px-xs py-sm text-term-red text-sm">
            Couldn’t load the explorer. It fills in when the next read succeeds.
          </p>
        ) : !ready ? (
          <p role="status" className="px-xs py-sm text-muted-foreground text-sm">
            Loading Mainloop…
          </p>
        ) : (
          <ExplorerTree
            nodes={nodes}
            isExpanded={(id) => isExpanded(prefs, id)}
            onToggle={onToggle}
            onOpen={onOpen}
            onAsk={onAsk}
            current={current}
          />
        )}
      </div>
    </section>
  );
}

export interface ExplorerSidebarProps {
  prefs: ExplorerPrefs;
  update: ReturnType<typeof useExplorerPrefs>['update'];
  nodes: ExplorerNode[];
  ready: boolean;
  failed: boolean;
}

/**
 * Desktop: the sidebar on the left, between the activity bar and the page. It shows one view of the
 * tree at a time (Sessions or Projects, chosen in the activity bar), is resizable from its right
 * edge, and is hidden from the activity bar or with the sidebar shortcut (⌘B on a Mac, Ctrl+B elsewhere). The shell owns the prefs and the
 * data, since the activity bar and the status bar read them too.
 */
export function ExplorerSidebar({ prefs, update, nodes, ready, failed }: ExplorerSidebarProps) {
  const { open, ask, toggle } = useTree(update);
  const drag = useRef<{ startX: number; startWidth: number; width: number } | null>(null);
  // The width while a drag is in progress. It is drawn from here and saved only when the drag ends.
  const [dragWidth, setDragWidth] = useState<number | null>(null);
  const view: SidebarView = prefs.view;
  const title = SIDEBAR_VIEWS.find(({ id }) => id === view)?.label ?? 'Explorer';

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
      aria-label={`${title} sidebar`}
      data-testid="explorer-aside"
      data-view={view}
      style={{ width: dragWidth ?? prefs.width }}
      className="border-border-subtle relative flex min-h-0 shrink-0 flex-col border-r"
    >
      <ExplorerBody
        nodes={viewNodes(nodes, view)}
        ready={ready}
        failed={failed}
        prefs={prefs}
        onToggle={toggle}
        onOpen={open}
        onAsk={ask}
        title={title}
        onCollapse={() => update((prev) => ({ ...prev, collapsed: true }))}
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

/** Phone: the same tree, full width, in the Explorer tab. */
export function ExplorerPane({
  lastVisit = null,
  title = 'Explorer',
  actions
}: {
  lastVisit?: number | null;
  /** The pane's heading: the tab's own label. */
  title?: string;
  /** Controls at the end of the header: health and the theme, which have no header of their own on a phone. */
  actions?: ReactNode;
}) {
  const { prefs, update } = useExplorerPrefs();
  const { nodes, ready, failed } = useExplorer(lastVisit);
  const { open, ask, toggle } = useTree(update);
  return (
    <ExplorerBody
      nodes={nodes}
      ready={ready}
      failed={failed}
      prefs={prefs}
      onToggle={toggle}
      onOpen={open}
      onAsk={ask}
      title={title}
      actions={actions}
    />
  );
}

/** The Needs-you count the Explorer tab shows. Shares the explorer's read, so the number matches the tree. */
export function useNeedsCount(): number {
  return needsCount(useExplorer().nodes);
}
