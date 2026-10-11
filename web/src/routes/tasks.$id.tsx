import { createFileRoute } from '@tanstack/react-router';
import { RoutePlaceholder } from '@/shell/RoutePlaceholder';

export const Route = createFileRoute('/tasks/$id')({
  component: Task
});

function Task() {
  const { id } = Route.useParams();
  return <RoutePlaceholder title="Task" slice="F6" detail={`${id}: not built yet (F6).`} />;
}
