import { ChevronDown, ChevronRight, MessageSquarePlus } from 'lucide-react';
import { useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { cn } from '@/components/ui/cn';
import type { Tone } from '@/lib/taskState';
import { visibleRows, type ExplorerNode, type Target } from './model';

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
      return (
        <li
          key={node.id}
          role="treeitem"
          aria-level={depth}
          aria-setsize={level.length}
          aria-posinset={index + 1}
          aria-selected={focused}
          aria-expanded={expandable ? open : undefined}
          aria-label={node.count !== undefined ? `${node.label}, ${node.count}` : undefined}
          tabIndex={focused ? 0 : -1}
          data-node-id={node.id}
          ref={(el) => {
            if (el) items.current.set(node.id, el);
            else items.current.delete(node.id);
          }}
          onFocus={() => setFocusId(node.id)}
          onKeyDown={(event) => onKeyDown(event, node, parentId)}
          className="outline-none"
        >
          {/* Mouse convenience only: the keyboard path is on the treeitem above, which owns Enter and the arrow keys. */}
          {/* eslint-disable-next-line jsx-a11y/click-events-have-key-events, jsx-a11y/no-static-element-interactions */}
          <div
            className={cn(
              'group gap-xs px-xs flex min-h-8 cursor-default items-center text-sm',
              'hover:bg-accent hover:text-accent-foreground',
              focused && 'ring-ring ring-1 ring-inset',
              node.note && 'text-muted-foreground italic'
            )}
            style={{ paddingLeft: `${(depth - 1) * 12 + 4}px` }}
            onClick={() => {
              setFocusId(node.id);
              activate(node);
            }}
          >
            <span
              aria-hidden="true"
              className="inline-flex w-4 shrink-0 justify-center"
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
            <span className="min-w-0 flex-1 truncate">{node.label}</span>
            {node.detail ? (
              <span className="text-muted-foreground hidden max-w-[9rem] shrink-0 truncate text-xs md:inline">
                {node.detail}
              </span>
            ) : null}
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
                // Hover and focus reveal it on desktop; phones have no hover, so it stays visible.
                className="text-muted-foreground hover:text-foreground inline-flex size-6 shrink-0 items-center justify-center opacity-100 focus-visible:opacity-100 md:opacity-0 md:group-hover:opacity-100"
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
    <ul role="tree" aria-label="Mainloop explorer" className="m-0 list-none p-0">
      {renderLevel(nodes, null, 1)}
    </ul>
  );
}
