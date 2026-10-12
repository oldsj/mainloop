import { createFileRoute } from '@tanstack/react-router';
import { WorkspacePage } from '@/features/workspaces';

export const Route = createFileRoute('/workspaces/$id')({
  head: () => ({ meta: [{ title: 'Workspace · Mainloop' }] }),
  component: Workspace
});

function Workspace() {
  const { id } = Route.useParams();
  // Keyed so that moving to another workspace remounts the page, and its notice and pending actions do not carry over.
  return <WorkspacePage key={id} id={id} />;
}
