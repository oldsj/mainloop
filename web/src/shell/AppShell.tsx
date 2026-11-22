import { Outlet } from '@tanstack/react-router';
import { useEffect, useState } from 'react';
import { useRecordVisit } from '@/features/home';
import { isShortcut } from '@/lib/platform';
import { CommandPalette } from './CommandPalette';
import { ConnectionBanner } from './ConnectionBanner';
import { CompactShell } from './CompactShell';
import { DesktopShell } from './DesktopShell';
import { resolveTab, useShellView, type ShellSearch, type View } from './view';

/** Picks the desktop or compact layout from `?view=` or the viewport. */
export function AppShell({ view, tab }: { view?: View; tab?: ShellSearch['tab'] }) {
  const resolved = useShellView(view);
  // The shell is on every route, so opening the app anywhere records the owner's visit, once.
  useRecordVisit();
  // The command palette belongs to the whole shell. ⌘K on a Mac, Ctrl+K elsewhere, never both on one
  // platform (the same rule as the sidebar's shortcut): on a Mac, Ctrl+K in a text field is the
  // system's "delete to the end of the line" and is left alone. It works while typing.
  const [palette, setPalette] = useState(false);
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (!isShortcut(event, 'k')) return;
      event.preventDefault();
      setPalette((open) => !open);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);
  const search = () => setPalette(true);
  // The banner is the first row of the screen-high column, so it takes height from the shell
  // instead of pushing the composer and tab bar off screen. The top and side insets keep the header
  // clear of a notch or a rounded corner; the bottom inset belongs to the tab bar.
  return (
    <div className="flex h-dvh flex-col pt-[env(safe-area-inset-top)] pr-[env(safe-area-inset-right)] pl-[env(safe-area-inset-left)]">
      {/* The desktop shell says "offline" in its status bar, so nothing shifts; the phone has no
          status bar and keeps the one-line banner. */}
      {resolved === 'desktop' ? null : <ConnectionBanner />}
      {resolved === 'desktop' ? (
        <DesktopShell onSearch={search}>
          <Outlet />
        </DesktopShell>
      ) : (
        <CompactShell tab={resolveTab(tab)} onSearch={search}>
          <Outlet />
        </CompactShell>
      )}
      <CommandPalette open={palette} onOpenChange={setPalette} />
    </div>
  );
}
