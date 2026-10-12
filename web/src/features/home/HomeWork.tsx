import { useEffect, useState } from 'react';
import { useTaskList } from '@/features/tasks';
import { SinceStrip } from './SinceStrip';
import { sinceCounts } from './model';
import { readLastVisit, writeLastVisit } from './lastVisit';
import { WorkList } from './WorkList';

/**
 * Home's work surface: the "Since you were here" strip above the grouped work list. The previous
 * visit is read once on mount and this visit is written after, so a page left open keeps its
 * baseline and the next visit compares against this one.
 */
export function HomeWork() {
  const [lastVisit] = useState(readLastVisit);
  useEffect(() => writeLastVisit(), []);
  const tasks = useTaskList();
  return (
    <div data-testid="home-work" className="border-border min-w-0 border-b">
      <SinceStrip counts={sinceCounts(tasks.data ?? [], lastVisit)} />
      <WorkList lastVisit={lastVisit} />
    </div>
  );
}
