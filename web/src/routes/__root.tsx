import type { QueryClient } from '@tanstack/react-query';
import { createRootRouteWithContext } from '@tanstack/react-router';
import { AppShell } from '@/shell/AppShell';
import { LiveUpdates } from '@/shell/LiveUpdates';
import { shellSearchSchema } from '@/shell/view';

export const Route = createRootRouteWithContext<{ queryClient: QueryClient }>()({
  validateSearch: (search) => shellSearchSchema.parse(search),
  component: RootLayout
});

function RootLayout() {
  const { view, tab } = Route.useSearch();
  return (
    <>
      <LiveUpdates />
      <AppShell view={view} tab={tab} />
    </>
  );
}
