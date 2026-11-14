import { useNavigate } from '@tanstack/react-router';
import { Search } from 'lucide-react';
import { Dialog } from 'radix-ui';
import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent } from 'react';
import { cn } from '@/components/ui/cn';
import { useAsk, useExplorer, useOpenTarget } from '@/features/explorer';
import { isApplePlatform } from '@/lib/platform';
import { filterItems, paletteItems, type PaletteItem } from './palette';
import { useTheme } from './themes';

export interface CommandPaletteProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The shell's baseline for "Since you were here", passed on to the explorer's read. */
  lastVisit: number | null;
}

/**
 * The command palette: one field that jumps to any task, session, project, schedule or page, changes
 * the theme, or asks Mainloop. It opens with ⌘K on a Mac and Ctrl+K elsewhere (the shell owns the
 * shortcut), and lists the same things the explorer does.
 */
export function CommandPalette({ open, onOpenChange, lastVisit }: CommandPaletteProps) {
  // The palette is opened from state, so Radix has no trigger to return focus to. Remember what had
  // focus and return there, unless running an item moved the owner to another page.
  const opener = useRef<HTMLElement | null>(null);
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="bg-background/80 fixed inset-0 z-50" />
        <Dialog.Content
          aria-describedby={undefined}
          data-testid="command-palette"
          onOpenAutoFocus={() => {
            opener.current =
              document.activeElement instanceof HTMLElement ? document.activeElement : null;
          }}
          onCloseAutoFocus={(event) => {
            event.preventDefault();
            if (opener.current?.isConnected) opener.current.focus();
          }}
          className="inset-x-md border-border bg-popover text-popover-foreground fixed top-24 z-50 mx-auto flex max-h-[70dvh] max-w-xl flex-col rounded-none border font-sans"
        >
          <Dialog.Title className="sr-only">Search and commands</Dialog.Title>
          <PaletteBody lastVisit={lastVisit} close={() => onOpenChange(false)} />
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

/** Mounted only while the palette is open, so its read and its state start fresh each time. */
function PaletteBody({ lastVisit, close }: { lastVisit: number | null; close: () => void }) {
  const { nodes } = useExplorer(lastVisit);
  const openTarget = useOpenTarget();
  const ask = useAsk();
  const navigate = useNavigate();
  const [, setTheme] = useTheme();
  const [query, setQuery] = useState('');
  const [picked, setPicked] = useState(0);
  const listId = useId();

  const items = useMemo(() => paletteItems(nodes), [nodes]);
  const shown = useMemo(() => filterItems(items, query), [items, query]);
  // The list changes under the highlight as the owner types or data arrives; keep it on a real row.
  const active = Math.min(picked, Math.max(0, shown.length - 1));
  const optionId = (index: number) => `${listId}-${index}`;

  useEffect(() => {
    document.getElementById(`${listId}-${active}`)?.scrollIntoView?.({ block: 'nearest' });
  }, [active, listId]);

  function run(item: PaletteItem) {
    const { action } = item;
    switch (action.do) {
      case 'open':
        openTarget(action.target);
        break;
      case 'settings':
        void navigate({ to: '/settings', search: {} });
        break;
      case 'schedules':
        void navigate({ to: '/schedules', search: {} });
        break;
      case 'theme':
        setTheme(action.theme);
        break;
      case 'ask':
        ask(action.text);
        break;
    }
    close();
  }

  function onKeyDown(event: KeyboardEvent<HTMLInputElement>) {
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      setPicked(shown.length === 0 ? 0 : (active + 1) % shown.length);
    } else if (event.key === 'ArrowUp') {
      event.preventDefault();
      setPicked(shown.length === 0 ? 0 : (active - 1 + shown.length) % shown.length);
    } else if (event.key === 'Enter') {
      const item = shown[active];
      if (!item) return;
      event.preventDefault();
      // The platform's modifier with Enter asks Mainloop about the row, when it can be asked about.
      const modified = isApplePlatform() ? event.metaKey : event.ctrlKey;
      if (modified && item.ask) {
        ask(item.ask);
        close();
      } else {
        run(item);
      }
    }
  }

  return (
    <>
      <div className="gap-sm px-sm border-border-subtle flex items-center border-b">
        <Search aria-hidden="true" className="text-muted-foreground size-4 shrink-0" />
        <input
          role="combobox"
          aria-label="Search tasks, sessions, projects and commands"
          aria-expanded="true"
          aria-controls={listId}
          aria-activedescendant={shown.length > 0 ? optionId(active) : undefined}
          aria-autocomplete="list"
          autoComplete="off"
          spellCheck={false}
          placeholder="Jump to a task, session or project, or type a command"
          value={query}
          onChange={(event) => {
            setQuery(event.target.value);
            setPicked(0);
          }}
          onKeyDown={onKeyDown}
          className="placeholder:text-muted-foreground text-ui h-11 min-w-0 flex-1 bg-transparent outline-none"
        />
      </div>
      <ul
        id={listId}
        role="listbox"
        aria-label="Results"
        className="m-0 min-h-0 flex-1 list-none overflow-y-auto p-0"
      >
        {shown.map((item, index) => (
          // Mouse and touch only: the keyboard path is the field above, which owns the arrows and Enter.
          // eslint-disable-next-line jsx-a11y/click-events-have-key-events
          <li
            key={item.id}
            id={optionId(index)}
            role="option"
            aria-selected={index === active}
            // The field keeps the focus, so a press must not take it; the click runs the row.
            onMouseDown={(event) => event.preventDefault()}
            onMouseMove={() => setPicked(index)}
            onClick={() => run(item)}
            className={cn(
              'gap-sm px-sm min-h-control text-ui flex cursor-default items-center',
              index === active && 'bg-accent text-accent-foreground'
            )}
          >
            <span className="min-w-0 flex-none basis-auto truncate">{item.label}</span>
            <span className="text-muted-foreground min-w-0 flex-1 truncate text-xs">
              {item.detail}
            </span>
            <span className="text-muted-foreground text-2xs shrink-0 tracking-wide uppercase">
              {item.kind}
            </span>
          </li>
        ))}
      </ul>
      {shown.length === 0 ? (
        <p role="status" className="text-muted-foreground p-md text-sm">
          Nothing to show yet.
        </p>
      ) : null}
      <p className="text-muted-foreground border-border-subtle px-sm text-2xs gap-md hidden border-t py-1 lg:flex">
        <span>↑↓ move</span>
        <span>Enter open</span>
        <span>{isApplePlatform() ? '⌘' : 'Ctrl+'}Enter ask Mainloop about it</span>
        <span>Esc close</span>
      </p>
    </>
  );
}
