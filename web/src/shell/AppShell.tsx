import { Outlet } from '@tanstack/react-router';
import { useLastVisit } from '@/features/home';
import { ConnectionBanner } from './ConnectionBanner';
import { CompactShell } from './CompactShell';
import { DesktopShell } from './DesktopShell';
import { resolveTab, useShellView, type ShellSearch, type View } from './view';

/** Picks the desktop or compact layout from `?view=` or the viewport. */
export function AppShell({ view, tab }: { view?: View; tab?: ShellSearch['tab'] }) {
  const resolved = useShellView(view);
  // One baseline for the whole shell: both layouts compare against the same previous visit.
  const lastVisit = useLastVisit();
  // The banner is the first row of the screen-high column, so it takes height from the shell
  // instead of pushing the composer and tab bar off screen. The top and side insets keep the header
  // clear of a notch or a rounded corner; the bottom inset belongs to the tab bar.
  return (
    <div className="flex h-dvh flex-col pt-[env(safe-area-inset-top)] pr-[env(safe-area-inset-right)] pl-[env(safe-area-inset-left)]">
      <ConnectionBanner />
      {resolved === 'desktop' ? (
        <DesktopShell lastVisit={lastVisit}>
          <Outlet />
        </DesktopShell>
      ) : (
        <CompactShell tab={resolveTab(tab)} lastVisit={lastVisit}>
          <Outlet />
        </CompactShell>
      )}
    </div>
  );
}
