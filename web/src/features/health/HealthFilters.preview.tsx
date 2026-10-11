import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider
} from '@tanstack/react-router';
import { useState } from 'react';
import { HealthFilters } from './HealthFilters';

/**
 * The form needs a router for Apply and Clear. A memory history keeps those inside the preview,
 * and the form is set to a narrowed view so the selected values show.
 */
function previewRouter() {
  return createRouter({
    routeTree: createRootRoute({
      component: () => (
        <div className="p-md">
          <HealthFilters
            id="health-filters"
            search={{ hours: 168, task_id: 'task-1' }}
            projects={[{ value: 'project-1', label: 'Mainloop' }]}
            tasks={[{ value: 'task-1', label: 'Add retry button' }]}
            sessions={[]}
            attempts={[]}
          />
        </div>
      )
    }),
    history: createMemoryHistory({ initialEntries: ['/'] })
  });
}

export default function HealthFiltersPreview() {
  const [router] = useState(previewRouter);
  return <RouterProvider router={router} />;
}
