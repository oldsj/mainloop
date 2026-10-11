import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';
import { RoutePlaceholder } from '@/shell/RoutePlaceholder';

// Filters are held in the URL (F8 adds the window and applies them).
const optionalString = z.string().optional().catch(undefined);
const healthSearch = z.object({
  project_id: optionalString,
  task_id: optionalString,
  session_id: optionalString,
  attempt_id: optionalString,
  provider: optionalString,
  hours: z.coerce.number().int().positive().optional().catch(undefined)
});

export const Route = createFileRoute('/health')({
  validateSearch: (search) => healthSearch.parse(search),
  component: () => <RoutePlaceholder title="Orchestration health" slice="F8" />
});
