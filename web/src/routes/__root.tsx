import type { QueryClient } from '@tanstack/react-query';
import {
  createRootRouteWithContext,
  HeadContent,
  redirect,
  retainSearchParams
} from '@tanstack/react-router';
import { AppShell } from '@/shell/AppShell';
import { LiveUpdates } from '@/shell/LiveUpdates';
import { RouteFocus } from '@/shell/RouteFocus';
import { shellSearchSchema } from '@/shell/view';

export const Route = createRootRouteWithContext<{ queryClient: QueryClient }>()({
  validateSearch: (search) => shellSearchSchema.parse(search),
  // `?view=` is a session-wide override, so links that omit it keep it.
  search: { middlewares: [retainSearchParams(['view'])] },
  // Links from before the explorer send `?tab=inbox`; the answer view now lives at `/inbox`.
  beforeLoad: ({ search }) => {
    if (search.tab === 'inbox')
      throw redirect({ to: '/inbox', search: { view: search.view }, replace: true });
  },
  component: RootLayout
});

function RootLayout() {
  const { view, tab } = Route.useSearch();
  return (
    <>
      <HeadContent />
      <LiveUpdates />
      <RouteFocus />
      <AppShell view={view} tab={tab} />
    </>
  );
}
