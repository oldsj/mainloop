import { PanelRightClose, PanelRightOpen } from 'lucide-react';
import { useCallback, useRef, useState, type KeyboardEvent, type PointerEvent } from 'react';
import { Button } from '@/components/ui/button';
import { badgeText } from '@/features/inbox/model';
import { needsCount, type ExplorerNode } from './model';
import {
  RAIL_WIDTH,
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
function useExplorerPrefs() {
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
  /** Desktop only: folds the panel to its rail. */
  onCollapse?: () => void;
}

/** The header, the one-line hint and the tree. Shared by the desktop aside and the phone pane. */
function ExplorerBody({
  nodes,
  ready,
  failed,
  prefs,
  onToggle,
  onOpen,
  onAsk,
  onCollapse
}: BodyProps) {
  return (
    <section aria-labelledby="explorer-heading" className="flex min-h-0 flex-1 flex-col">
      <header className="gap-xs px-md py-sm border-border flex flex-col border-b">
        <div className="gap-sm flex items-center justify-between">
          <h2 id="explorer-heading" className="text-sm font-medium">
            Explorer
          </h2>
          {onCollapse ? (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              aria-label="Collapse explorer"
              onClick={onCollapse}
            >
              <PanelRightClose aria-hidden="true" />
            </Button>
          ) : null}
        </div>
        <p className="text-muted-foreground text-xs">
          Ask Mainloop to start, stop or change anything.
        </p>
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
          />
        )}
      </div>
    </section>
  );
}

/** Desktop: the explorer down the right of the main chat. Resizable, and folds to a rail. */
export function ExplorerAside() {
  const { prefs, update } = useExplorerPrefs();
  const { nodes, ready, failed } = useExplorer();
  const { open, ask, toggle } = useTree(update);
  const drag = useRef<{ startX: number; startWidth: number } | null>(null);
  const needs = needsCount(nodes);

  const setWidth = (width: number) => update((prev) => ({ ...prev, width: clampWidth(width) }));

  const onPointerDown = (event: PointerEvent<HTMLDivElement>) => {
    event.currentTarget.setPointerCapture(event.pointerId);
    drag.current = { startX: event.clientX, startWidth: prefs.width };
  };
  // The panel sits on the right, so dragging left widens it.
  const onPointerMove = (event: PointerEvent<HTMLDivElement>) => {
    const start = drag.current;
    if (!start) return;
    const width = clampWidth(start.startWidth + (start.startX - event.clientX));
    // Each move is a render only; the width is written once, when the drag ends.
    update((prev) => (prev.width === width ? prev : { ...prev, width }));
  };
  const onPointerUp = () => {
    drag.current = null;
  };
  const onHandleKey = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === 'ArrowLeft') setWidth(prefs.width + RESIZE_STEP);
    else if (event.key === 'ArrowRight') setWidth(prefs.width - RESIZE_STEP);
    else return;
    event.preventDefault();
  };

  if (prefs.collapsed) {
    return (
      <aside
        aria-label="Explorer"
        data-testid="explorer-rail"
        style={{ width: RAIL_WIDTH }}
        className="border-border gap-sm py-sm flex shrink-0 flex-col items-center border-l"
      >
        <Button
          type="button"
          variant="ghost"
          size="icon"
          aria-label={needs > 0 ? `Show explorer, ${needs} need you` : 'Show explorer'}
          aria-expanded={false}
          onClick={() => update((prev) => ({ ...prev, collapsed: false }))}
        >
          <PanelRightOpen aria-hidden="true" />
        </Button>
        {needs > 0 ? (
          <span
            aria-hidden="true"
            className="border-term-yellow text-term-yellow bg-background inline-flex min-w-5 items-center justify-center border px-1 text-xs tabular-nums"
          >
            {badgeText(needs)}
          </span>
        ) : null}
      </aside>
    );
  }

  return (
    <aside
      aria-label="Explorer"
      data-testid="explorer-aside"
      style={{ width: prefs.width }}
      className="border-border relative flex min-h-0 shrink-0 flex-col border-l"
    >
      {/* A focusable window splitter (WAI-ARIA): the separator takes keys, so it is not a plain static element. */}
      {/* eslint-disable jsx-a11y/no-noninteractive-element-interactions, jsx-a11y/no-noninteractive-tabindex */}
      <div
        role="separator"
        aria-orientation="vertical"
        aria-label="Resize explorer"
        aria-valuemin={WIDTH_MIN}
        aria-valuemax={WIDTH_MAX}
        aria-valuenow={prefs.width}
        tabIndex={0}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
        onKeyDown={onHandleKey}
        className="hover:bg-accent focus-visible:bg-accent absolute inset-y-0 left-0 z-10 w-1 cursor-col-resize touch-none outline-none"
      />
      {/* eslint-enable jsx-a11y/no-noninteractive-element-interactions, jsx-a11y/no-noninteractive-tabindex */}
      <ExplorerBody
        nodes={nodes}
        ready={ready}
        failed={failed}
        prefs={prefs}
        onToggle={toggle}
        onOpen={open}
        onAsk={ask}
        onCollapse={() => update((prev) => ({ ...prev, collapsed: true }))}
      />
    </aside>
  );
}

/** Phone: the same tree, full width, in the Explorer tab. */
export function ExplorerPane() {
  const { prefs, update } = useExplorerPrefs();
  const { nodes, ready, failed } = useExplorer();
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
    />
  );
}

/** The Needs-you count the Explorer tab shows. Shares the explorer's read, so the number matches the tree. */
export function useNeedsCount(): number {
  return needsCount(useExplorer().nodes);
}
