import { createFileRoute } from '@tanstack/react-router';
import { TaskPage } from '@/features/tasks';

export const Route = createFileRoute('/tasks/$id')({
  component: Task
});

function Task() {
  const { id } = Route.useParams();
  // Keyed so that moving to another task remounts the page, and its notice and pending requests do not carry over.
  return <TaskPage key={id} id={id} />;
}
