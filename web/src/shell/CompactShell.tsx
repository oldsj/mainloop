import { Link, useNavigate } from '@tanstack/react-router';
import { Inbox, ListTree, MessageSquare, Plus } from 'lucide-react';
import type { ReactNode } from 'react';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { InboxPanel, SessionsPanel } from './slots';
import { ThemeSelector } from './ThemeSelector';
import type { Tab } from './view';

const TAB_ITEMS = [
  { tab: 'chat', label: 'Chat', Icon: MessageSquare },
  { tab: 'sessions', label: 'Sessions', Icon: ListTree },
  { tab: 'inbox', label: 'Inbox', Icon: Inbox }
] as const satisfies readonly { tab: Tab; label: string; Icon: unknown }[];

/** <1024 px: one pane at a time, switched by a bottom tab bar. The tab lives in the URL. */
export function CompactShell({ tab, children }: { tab: Tab; children: ReactNode }) {
  const navigate = useNavigate();
  return (
    <div data-testid="compact-shell" className="flex h-dvh flex-col">
      <header className="gap-sm border-border px-md flex items-center border-b">
        <Link
          to="/"
          search={(prev) => ({ ...prev, tab: undefined })}
          className="py-sm text-primary text-sm font-semibold"
        >
          $ mainloop
        </Link>
        <nav className="gap-xs ml-auto flex items-center" aria-label="Primary">
          <Link
            to="/agents"
            data-testid="header-new-agent"
            aria-label="New agent session"
            className={cn(buttonVariants({ variant: 'ghost', size: 'icon' }))}
          >
            <Plus aria-hidden="true" />
          </Link>
          <ThemeSelector />
        </nav>
      </header>
      <main className="min-h-0 flex-1 overflow-y-auto">
        {tab === 'chat' && children}
        {tab === 'sessions' && <SessionsPanel />}
        {tab === 'inbox' && <InboxPanel />}
      </main>
      <nav
        data-testid="mobile-tab-bar"
        aria-label="Sections"
        className="border-border flex border-t pb-[env(safe-area-inset-bottom)]"
      >
        {TAB_ITEMS.map(({ tab: id, label, Icon }) => (
          <button
            key={id}
            type="button"
            data-testid={`tab-${id}`}
            aria-label={label}
            aria-pressed={tab === id}
            onClick={() =>
              void navigate({
                to: '.',
                search: (prev: Record<string, unknown>) => ({
                  ...prev,
                  tab: id === 'chat' ? undefined : id
                })
              })
            }
            className={cn(
              'gap-xs flex min-h-11 flex-1 flex-col items-center justify-center text-xs',
              tab === id ? 'bg-accent text-primary' : 'text-muted-foreground'
            )}
          >
            <Icon className="size-4" aria-hidden="true" />[{label.toUpperCase()}]
          </button>
        ))}
      </nav>
    </div>
  );
}
