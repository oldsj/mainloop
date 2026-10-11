import { createFileRoute } from '@tanstack/react-router';
import { ScheduleCreatePage } from '@/features/schedules';

export const Route = createFileRoute('/schedules/new')({
  component: ScheduleCreatePage
});
