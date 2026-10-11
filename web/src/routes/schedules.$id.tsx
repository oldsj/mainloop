import { createFileRoute } from '@tanstack/react-router';
import { ScheduleDetailPage } from '@/features/schedules';

export const Route = createFileRoute('/schedules/$id')({
  component: Schedule
});

function Schedule() {
  const { id } = Route.useParams();
  // Keyed so that moving to another schedule remounts the page, and its form and notices do not carry over.
  return <ScheduleDetailPage key={id} id={id} />;
}
