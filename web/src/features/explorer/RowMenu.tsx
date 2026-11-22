import { useEffect, useLayoutEffect, useRef, useState, type KeyboardEvent } from 'react';

/** The menu keeps this far from the viewport edge. */
const EDGE_PX = 8;

export interface RowMenuAction {
  label: string;
  run: () => void;
}

/**
 * A row's actions in a small menu at the point it was asked for: right-click, or Shift+F10 and the
 * menu key. Escape, a press outside, a scroll of the list the row is in, or its row moving closes
 * it. Other scrolling, such as the chat following a turn, leaves it open. `onClose` is told whether
 * focus was in the menu, so it is returned to the row only then and never taken from the composer.
 */
export function RowMenu({
  label,
  x,
  y,
  rowId,
  actions,
  onClose
}: {
  label: string;
  x: number;
  y: number;
  /** The id (`data-node-id`) of the row the menu belongs to. */
  rowId: string;
  actions: RowMenuAction[];
  onClose: (restoreFocus: boolean) => void;
}) {
  const menu = useRef<HTMLDivElement>(null);
  const [place, setPlace] = useState({ left: x, top: y });

  // Kept inside the viewport, then the first action takes focus.
  useLayoutEffect(() => {
    const el = menu.current;
    if (!el) return;
    const { width, height } = el.getBoundingClientRect();
    setPlace({
      left: Math.max(EDGE_PX, Math.min(x, window.innerWidth - width - EDGE_PX)),
      top: Math.max(EDGE_PX, Math.min(y, window.innerHeight - height - EDGE_PX))
    });
    el.querySelector<HTMLElement>('[role="menuitem"]')?.focus();
  }, [x, y]);

  useEffect(() => {
    const focused = () => !!menu.current?.contains(document.activeElement);
    const anchor =
      Array.from(document.querySelectorAll<HTMLElement>('[data-node-id]')).find(
        (el) => el.dataset.nodeId === rowId
      )?.firstElementChild ?? null;
    const start = anchor?.getBoundingClientRect();
    const moved = () => {
      if (!anchor || !start) return false;
      if (!anchor.isConnected) return true;
      const now = anchor.getBoundingClientRect();
      return Math.abs(now.top - start.top) > 1 || Math.abs(now.left - start.left) > 1;
    };
    const outside = (event: Event) => {
      if (!menu.current?.contains(event.target as Node)) onClose(false);
    };
    // A scroll of something holding the row (the sidebar, or the page on a phone) moves it. Any
    // other scroll closes the menu only if it moved the row after all.
    const scroll = (event: Event) => {
      const target = event.target;
      const holdsRow = !!anchor && target instanceof Node && target.contains(anchor);
      if (holdsRow || moved()) onClose(focused());
    };
    const resize = () => onClose(focused());
    // Rows added or removed above it move the row without any scroll: its list changes height.
    const list = anchor?.closest('ul');
    const observer =
      list && typeof ResizeObserver !== 'undefined'
        ? new ResizeObserver(() => {
            if (moved()) onClose(focused());
          })
        : null;
    if (list) observer?.observe(list);
    window.addEventListener('pointerdown', outside, true);
    window.addEventListener('scroll', scroll, true);
    window.addEventListener('resize', resize);
    return () => {
      observer?.disconnect();
      window.removeEventListener('pointerdown', outside, true);
      window.removeEventListener('scroll', scroll, true);
      window.removeEventListener('resize', resize);
    };
  }, [rowId, onClose]);

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const items = Array.from(
      menu.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? []
    );
    const at = items.indexOf(document.activeElement as HTMLElement);
    if (event.key === 'Escape' || event.key === 'Tab') onClose(true);
    else if (event.key === 'ArrowDown') items[(at + 1) % items.length]?.focus();
    else if (event.key === 'ArrowUp') items[(at - 1 + items.length) % items.length]?.focus();
    else return;
    event.preventDefault();
    event.stopPropagation();
  };

  return (
    <div
      ref={menu}
      role="menu"
      aria-label={`Actions for ${label}`}
      tabIndex={-1}
      onKeyDown={onKeyDown}
      className="border-border bg-popover text-popover-foreground fixed z-50 flex min-w-40 flex-col border py-1 shadow-md"
      style={{ left: place.left, top: place.top }}
    >
      {actions.map((action) => (
        <button
          key={action.label}
          type="button"
          role="menuitem"
          tabIndex={-1}
          className="hover:bg-accent hover:text-accent-foreground focus-visible:bg-accent px-md min-h-control text-left text-sm outline-none"
          onClick={() => {
            onClose(true);
            action.run();
          }}
        >
          {action.label}
        </button>
      ))}
    </div>
  );
}
