/**
 * What waits on the owner and what Mainloop is handling, as one list for every place that shows or
 * counts it: the tree's groups, the status bar, the Inbox badge and the Inbox's own two views.
 *
 * `/attention` lists the owner's open actions. A task can also need a decision that `/attention`
 * does not list (a task that failed in the last seven days and can be retried, a pull request
 * waiting to be merged), so those tasks follow the listed items. Pure, so the rule tests without a
 * QueryClient.
 */
import { isHeld, workGroup } from '@/features/home/model';
import { useTaskList } from '@/features/tasks/queries';
import type { TaskView } from '@/lib/contract';
import { attentionItems, needsYouItems, problemItems, taskIdOf, type AttentionItem } from './model';
import { useAttention } from './queries';

export interface OwnerWork {
  /** Needs you: the listed owner actions, in the server's order. */
  needs: AttentionItem[];
  /** Needs you: tasks that need a decision and that no listed item names, newest first. */
  needsTasks: TaskView[];
  /** Problems: Mainloop's own recovery work, as listed. */
  problems: AttentionItem[];
  /** Problems: tasks Mainloop is recovering that no listed item names, newest first. */
  problemTasks: TaskView[];
  /** The tasks a listed item already names, by its task id or its `/tasks/<id>` link. */
  named: ReadonlySet<string>;
}

export function ownerWork(
  attention: AttentionItem[],
  tasks: TaskView[],
  now: number = Date.now()
): OwnerWork {
  const named = new Set<string>();
  for (const item of attention) {
    const taskId = taskIdOf(item.task_id, item.href);
    if (taskId) named.add(taskId);
  }
  const needsTasks: TaskView[] = [];
  const problemTasks: TaskView[] = [];
  const newestFirst = [...tasks].sort(
    (a, b) => Date.parse(b.task.updated_at) - Date.parse(a.task.updated_at)
  );
  for (const view of newestFirst) {
    if (named.has(view.task.id)) continue;
    const group = workGroup(view, null, now);
    // A task the main thread set aside is out of the owner's way until its time passes.
    if ((group === 'needs-you' || group === 'blocked') && !isHeld(view, now)) needsTasks.push(view);
    if (group === 'problem') problemTasks.push(view);
  }
  return {
    needs: needsYouItems(attention),
    needsTasks,
    problems: problemItems(attention),
    problemTasks,
    named
  };
}

/** How many things need the owner: every Needs-you badge and heading shows this number. */
export function needsTotal(work: OwnerWork): number {
  return work.needs.length + work.needsTasks.length;
}

/** How many problems Mainloop is handling. */
export function problemTotal(work: OwnerWork): number {
  return work.problems.length + work.problemTasks.length;
}

/** The same list from the two reads the shell already keeps fresh. */
export function useOwnerWork(): OwnerWork {
  const attention = useAttention();
  const tasks = useTaskList();
  return ownerWork(attentionItems(attention.data), tasks.data ?? []);
}
