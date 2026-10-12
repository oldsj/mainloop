import { useTaskList } from '@/features/tasks';
import { SinceStrip } from './SinceStrip';
import { sinceCounts } from './model';

/** The "Since you were here" strip, counted from the task list against the last visit. */
export function HomeStrip({
  lastVisit,
  compact = false
}: {
  lastVisit: number | null;
  compact?: boolean;
}) {
  const tasks = useTaskList();
  return <SinceStrip counts={sinceCounts(tasks.data ?? [], lastVisit)} compact={compact} />;
}
