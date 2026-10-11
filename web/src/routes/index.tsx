import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';
import { RoutePlaceholder } from '@/shell/RoutePlaceholder';

export const Route = createFileRoute('/')({
  // `hitl` puts that approval card at the top of the thread (F2, F3).
  validateSearch: (search) =>
    z.object({ hitl: z.string().optional().catch(undefined) }).parse(search),
  component: () => <RoutePlaceholder title="Main thread" slice="F2" testId="main-thread" />
});
