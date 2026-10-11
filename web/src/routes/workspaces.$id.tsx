import { createFileRoute } from '@tanstack/react-router';
import { RoutePlaceholder } from '@/shell/RoutePlaceholder';

export const Route = createFileRoute('/workspaces/$id')({
  component: Workspace
});

function Workspace() {
  const { id } = Route.useParams();
  return <RoutePlaceholder title="Workspace" slice="F8" detail={`${id}: not built yet (F8).`} />;
}
