import { createFileRoute } from '@tanstack/react-router';
import { RoutePlaceholder } from '@/shell/RoutePlaceholder';

export const Route = createFileRoute('/agents')({
  component: () => <RoutePlaceholder title="New agent session" slice="F4" />
});
