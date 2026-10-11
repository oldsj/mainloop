import { createFileRoute } from '@tanstack/react-router';
import { ScheduleListPage } from '@/features/schedules';

export const Route = createFileRoute('/schedules/')({
  component: ScheduleListPage
});
