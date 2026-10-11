import { Outlet } from '@tanstack/react-router';
import { ConnectionBanner } from './ConnectionBanner';
import { CompactShell } from './CompactShell';
import { DesktopShell } from './DesktopShell';
import { resolveView, useIsWide, type Tab, type View } from './view';

/** Picks the desktop or compact layout from `?view=` or the viewport. */
export function AppShell({ view, tab }: { view?: View; tab?: Tab }) {
  const isWide = useIsWide();
  const resolved = resolveView(view, isWide);
  return (
    <>
      <ConnectionBanner />
      {resolved === 'desktop' ? (
        <DesktopShell>
          <Outlet />
        </DesktopShell>
      ) : (
        <CompactShell tab={tab ?? 'chat'}>
          <Outlet />
        </CompactShell>
      )}
    </>
  );
}
