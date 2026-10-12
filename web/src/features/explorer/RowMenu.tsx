import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type PointerEvent
} from 'react';

/** How long a finger rests on a row before its menu opens. */
export const LONG_PRESS_MS = 500;
/** A finger that moves further than this is scrolling, not pressing. */
const SLOP_PX = 10;
/** The menu keeps this far from the viewport edge. */
const EDGE_PX = 8;

export interface RowMenuAction {
  label: string;
  run: () => void;
}

/**
 * A row's actions in a small menu at the point it was asked for: right-click on desktop, a long
 * press on the phone, or Shift+F10 and the menu key. Escape, a press outside or a scroll closes it,
 * and `onClose` returns focus to the row.
 */
export function RowMenu({
  label,
  x,
  y,
  actions,
  onClose
}: {
  label: string;
  x: number;
  y: number;
  actions: RowMenuAction[];
  onClose: () => void;
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
    const outside = (event: Event) => {
      if (!menu.current?.contains(event.target as Node)) onClose();
    };
    window.addEventListener('pointerdown', outside, true);
    window.addEventListener('scroll', onClose, true);
    window.addEventListener('resize', onClose);
    return () => {
      window.removeEventListener('pointerdown', outside, true);
      window.removeEventListener('scroll', onClose, true);
      window.removeEventListener('resize', onClose);
    };
  }, [onClose]);

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const items = Array.from(
      menu.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? []
    );
    const at = items.indexOf(document.activeElement as HTMLElement);
    if (event.key === 'Escape' || event.key === 'Tab') onClose();
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
          className="hover:bg-accent hover:text-accent-foreground focus-visible:bg-accent px-md min-h-11 text-left text-sm outline-none lg:min-h-7"
          onClick={() => {
            onClose();
            action.run();
          }}
        >
          {action.label}
        </button>
      ))}
    </div>
  );
}

/**
 * A long press on a touch screen. It calls `onPress` with the row and the finger's point after `LONG_PRESS_MS`,
 * unless the finger lifts or moves first, and `pressed()` then tells the click that follows to do
 * nothing. Mouse and pen are left to `contextmenu`.
 */
export function useLongPress(onPress: (key: string, x: number, y: number) => void) {
  const timer = useRef<number | null>(null);
  const start = useRef({ x: 0, y: 0 });
  const fired = useRef(false);

  const cancel = () => {
    if (timer.current !== null) window.clearTimeout(timer.current);
    timer.current = null;
  };
  useEffect(() => cancel, []);

  return {
    /** The handlers for one row, named by `key`. */
    handlers: (key: string) => ({
      onPointerDown: (event: PointerEvent) => {
        fired.current = false;
        if (event.pointerType !== 'touch') return;
        const { clientX: x, clientY: y } = event;
        start.current = { x, y };
        cancel();
        timer.current = window.setTimeout(() => {
          timer.current = null;
          fired.current = true;
          onPress(key, x, y);
        }, LONG_PRESS_MS);
      },
      onPointerMove: (event: PointerEvent) => {
        if (timer.current === null) return;
        const dx = event.clientX - start.current.x;
        const dy = event.clientY - start.current.y;
        if (Math.hypot(dx, dy) > SLOP_PX) cancel();
      },
      onPointerUp: cancel,
      onPointerCancel: cancel
    }),
    /** True once, right after a long press opened the menu, so its trailing click is ignored. */
    pressed: () => {
      const was = fired.current;
      fired.current = false;
      return was;
    }
  };
}
