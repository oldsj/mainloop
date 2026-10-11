import { createFileRoute } from '@tanstack/react-router';
import { RoutePlaceholder } from '@/shell/RoutePlaceholder';

export const Route = createFileRoute('/projects/$id')({
  component: Project
});

function Project() {
  const { id } = Route.useParams();
  return <RoutePlaceholder title="Project" slice="F7" detail={`${id}: not built yet (F7).`} />;
}
