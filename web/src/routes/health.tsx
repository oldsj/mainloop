import { createFileRoute } from '@tanstack/react-router';
import { HealthPage, healthSearch } from '@/features/health';

// The filters are the URL's: an unreadable value is dropped to its default, not a failed page.
export const Route = createFileRoute('/health')({
  validateSearch: (search) => healthSearch.parse(search),
  head: () => ({ meta: [{ title: 'Health · Mainloop' }] }),
  component: Health
});

function Health() {
  const search = Route.useSearch();
  return <HealthPage search={search} />;
}
