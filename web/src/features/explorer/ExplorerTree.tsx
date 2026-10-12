import { ChevronDown, ChevronRight, MessageSquarePlus } from 'lucide-react';
import { useCallback, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { cn } from '@/components/ui/cn';
import type { Tone } from '@/lib/taskState';
import { visibleRows, type ExplorerNode, type Target } from './model';
import { RowMenu, useLongPress, type RowMenuAction } from './RowMenu';

/** The status dot's colour. Muted is the default, so an unknown state never reads as a problem. */
const DOT: Record<Tone, string> = {
  active: 'bg-term-cyan',
  attention: 'bg-term-yellow',
  ok: 'bg-term-green',
  bad: 'bg-term-red',
  muted: 'bg-term-fg-muted'
};

export interface ExplorerTreeProps {
  nodes: ExplorerNode[];
  isExpanded: (id: string) => boolean;
  onToggle: (id: string) => void;
  onOpen: (target: Target) => void;
  onAsk: (text: string) => void;
}

/**
 * The explorer tree: `role="tree"` with roving focus. Up and down move, right expands or steps into
 * the first child, left collapses or steps to the parent, Enter opens the row, and `a` asks Mainloop
 * about it. Each row is one treeitem, so a screen reader hears its level, position and state.
 * A row's actions (Open, Ask Mainloop) are also in a small menu: right-click on desktop, a long
 * press on the phone, or Shift+F10 and the menu key.
 */
export function ExplorerTree({ nodes, isExpanded, onToggle, onOpen, onAsk }: ExplorerTreeProps) {
  const rows = visibleRows(nodes, isExpanded);
  const [focusId, setFocusId] = useState<string | null>(null);
  const items = useRef(new Map<string, HTMLLIElement>());
  // The roving stop is the focused row if it is still on screen, otherwise the first row.
  const activeId = rows.some((row) => row.node.id === focusId)
    ? focusId
    : (rows[0]?.node.id ?? null);

  const focusRow = (id: string) => {
    setFocusId(id);
    items.current.get(id)?.focus();
  };

  const activate = (node: ExplorerNode) => {
    if (node.target) onOpen(node.target);
    else if (node.children.length > 0) onToggle(node.id);
  };

  const [menu, setMenu] = useState<{ id: string; x: number; y: number } | null>(null);
  const menuNode = menu ? rows.find((row) => row.node.id === menu.id)?.node : undefined;
  const openMenu = (node: ExplorerNode, x: number, y: number) => {
    if (actionsFor(node, onOpen, onAsk).length === 0) return false;
    setFocusId(node.id);
    setMenu({ id: node.id, x, y });
    return true;
  };
  const closeMenu = useCallback(() => {
    setMenu((open) => {
      if (open) items.current.get(open.id)?.focus();
      return null;
    });
  }, []);
  const longPress = useLongPress((id, x, y) => {
    const node = rows.find((row) => row.node.id === id)?.node;
    if (node) openMenu(node, x, y);
  });

  const onKeyDown = (
    event: KeyboardEvent<HTMLLIElement>,
    node: ExplorerNode,
    parentId: string | null
  ) => {
    const index = rows.findIndex((row) => row.node.id === node.id);
    const expandable = node.children.length > 0;
    const open = expandable && isExpanded(node.id);
    const next = rows[index + 1];
    const prev = index > 0 ? rows[index - 1] : undefined;
    const first = rows[0];
    const last = rows[rows.length - 1];
    switch (event.key) {
      case 'ArrowDown':
        if (next) focusRow(next.node.id);
        break;
      case 'ArrowUp':
        if (prev) focusRow(prev.node.id);
        break;
      case 'Home':
        if (first) focusRow(first.node.id);
        break;
      case 'End':
        if (last) focusRow(last.node.id);
        break;
      case 'ArrowRight':
        if (expandable && !open) onToggle(node.id);
        else if (open && node.children[0]) focusRow(node.children[0].id);
        break;
      case 'ArrowLeft':
        if (open) onToggle(node.id);
        else if (parentId) focusRow(parentId);
        break;
      case 'Enter':
        activate(node);
        break;
      case 'a':
        if (node.ask) onAsk(node.ask);
        break;
      case 'ContextMenu':
      case 'F10': {
        if (event.key === 'F10' && !event.shiftKey) return;
        const box = event.currentTarget.firstElementChild?.getBoundingClientRect();
        openMenu(node, (box?.left ?? 0) + 24, box?.bottom ?? 0);
        break;
      }
      default:
        return;
    }
    event.preventDefault();
    event.stopPropagation();
  };

  const renderLevel = (level: ExplorerNode[], parentId: string | null, depth: number): ReactNode =>
    level.map((node, index) => {
      const expandable = node.children.length > 0;
      const open = expandable && isExpanded(node.id);
      const focused = node.id === activeId;
      const name = [
        node.label,
        node.detail,
        node.count !== undefined ? String(node.count) : undefined
      ]
        .filter(Boolean)
        .join(', ');
      return (
        <li
          key={node.id}
          role="treeitem"
          aria-level={depth}
          aria-setsize={level.length}
          aria-posinset={index + 1}
          aria-selected={focused}
          aria-expanded={expandable ? open : undefined}
          aria-label={name}
          tabIndex={focused ? 0 : -1}
          data-node-id={node.id}
          ref={(el) => {
            if (el) items.current.set(node.id, el);
            else items.current.delete(node.id);
          }}
          // Focus from a child row bubbles to its parent's treeitem, so only the row itself takes the focus.
          onFocus={(event) => {
            if (event.target === event.currentTarget) setFocusId(node.id);
          }}
          onKeyDown={(event) => onKeyDown(event, node, parentId)}
          // The ring is drawn on this treeitem's own row (its first child) while the browser shows
          // keyboard focus on it, so it never lights a parent's row or a tree that was never focused.
          className="[&:focus-visible>div]:ring-ring outline-none [&:focus-visible>div]:ring-1 [&:focus-visible>div]:ring-inset"
        >
          {/* Mouse and touch only: the keyboard path is on the treeitem above, which owns Enter and the arrow keys. */}
          {/* eslint-disable-next-line jsx-a11y/click-events-have-key-events, jsx-a11y/no-static-element-interactions */}
          <div
            className={cn(
              // A phone row is a 44 px touch target with the status on a second line; desktop is one dense line.
              'group gap-xs px-xs relative flex min-h-11 cursor-default items-center py-1 text-sm lg:min-h-7 lg:py-0',
              // A long press opens the row menu, not the phone's own text selection or link callout.
              'select-none [-webkit-touch-callout:none]',
              'hover:bg-accent hover:text-accent-foreground',
              menu?.id === node.id && 'bg-accent text-accent-foreground',
              node.note && 'text-muted-foreground italic'
            )}
            style={{ paddingLeft: `${(depth - 1) * 12 + 4}px` }}
            {...longPress.handlers(node.id)}
            onContextMenu={(event) => {
              if (openMenu(node, event.clientX, event.clientY)) event.preventDefault();
            }}
            onClick={() => {
              // The finger lifting after a long press is not a tap on the row.
              if (longPress.pressed()) return;
              setFocusId(node.id);
              activate(node);
            }}
          >
            {/* One guide per ancestor, centred on that ancestor's chevron, so a deep row shows its line of descent. */}
            {Array.from({ length: depth - 1 }, (_, ancestor) => (
              <span
                key={ancestor}
                aria-hidden="true"
                data-testid="indent-guide"
                className="bg-border absolute inset-y-0 w-px"
                style={{ left: `${ancestor * 12 + 12}px` }}
              />
            ))}
            {/* On a phone the chevron's hit area grows to a touch target past its 16 px glyph. */}
            <span
              aria-hidden="true"
              className={cn(
                'relative inline-flex w-4 shrink-0 justify-center',
                expandable && 'before:absolute before:-inset-x-2 before:-inset-y-3 lg:before:hidden'
              )}
              onClick={(event) => {
                if (!expandable) return;
                event.stopPropagation();
                onToggle(node.id);
              }}
            >
              {expandable ? (
                open ? (
                  <ChevronDown className="size-3.5" />
                ) : (
                  <ChevronRight className="size-3.5" />
                )
              ) : null}
            </span>
            {node.tone ? (
              <span aria-hidden="true" className={cn('size-2 shrink-0', DOT[node.tone])} />
            ) : null}
            {/* The status sits under the label on a phone, which has no hover to reveal it, and beside it
                on desktop, where it gives way before the label on a narrow line. */}
            <span className="lg:gap-xs flex min-w-0 flex-1 flex-col lg:flex-row lg:items-baseline">
              <span className="min-w-0 truncate lg:flex-auto">{node.label}</span>
              {node.detail ? (
                <span className="text-muted-foreground min-w-0 truncate text-xs lg:shrink-[4] lg:text-right">
                  {node.detail}
                </span>
              ) : null}
            </span>
            {node.count !== undefined ? (
              <span
                className="text-muted-foreground pl-xs shrink-0 text-xs tabular-nums"
                aria-hidden="true"
              >
                {node.count}
              </span>
            ) : null}
            {node.ask ? (
              <button
                type="button"
                tabIndex={-1}
                aria-label={`Ask Mainloop about ${node.label}`}
                title="Ask Mainloop about this"
                onClick={(event) => {
                  event.stopPropagation();
                  onAsk(node.ask ?? '');
                }}
                // A phone has no hover, so it stays visible there as a touch target. Desktop shows it on
                // hover or while its row has keyboard focus.
                className="text-muted-foreground hover:text-foreground inline-flex size-11 shrink-0 items-center justify-center lg:size-6 lg:opacity-0 lg:group-hover:opacity-100 lg:[li:focus-visible>div>&]:opacity-100"
              >
                <MessageSquarePlus className="size-3.5" aria-hidden="true" />
              </button>
            ) : null}
          </div>
          {open ? (
            <ul role="group" className="m-0 list-none p-0">
              {renderLevel(node.children, node.id, depth + 1)}
            </ul>
          ) : null}
        </li>
      );
    });

  return (
    <>
      <ul role="tree" aria-label="Mainloop explorer" className="m-0 list-none p-0">
        {renderLevel(nodes, null, 1)}
      </ul>
      {menu && menuNode ? (
        <RowMenu
          label={menuNode.label}
          x={menu.x}
          y={menu.y}
          actions={actionsFor(menuNode, onOpen, onAsk)}
          onClose={closeMenu}
        />
      ) : null}
    </>
  );
}

/** A row's menu: Open when it leads somewhere, Ask Mainloop when it has a prefill. */
function actionsFor(
  node: ExplorerNode,
  onOpen: (target: Target) => void,
  onAsk: (text: string) => void
): RowMenuAction[] {
  const { target, ask } = node;
  return [
    ...(target ? [{ label: 'Open', run: () => onOpen(target) }] : []),
    ...(ask ? [{ label: 'Ask Mainloop', run: () => onAsk(ask) }] : [])
  ];
}
