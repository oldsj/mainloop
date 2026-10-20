/**
 * The explorer's data and its two actions. Reads reuse the queries the pages use, so SSE
 * invalidation reaches the tree without a second map. Opening a row and asking Mainloop both leave
 * the tree's tab, so on the phone the page or the composer shows in the chat pane.
 */
import { useNavigate } from '@tanstack/react-router';
import { useMemo } from 'react';
import { attentionItems } from '@/features/inbox/model';
import { useAttention } from '@/features/inbox/queries';
import { useAttentionPoll } from '@/features/inbox/poll';
import { keepValid, projectRowSchema } from '@/features/projects/model';
import { useProjects } from '@/features/projects/queries';
import { useSchedules } from '@/features/schedules/queries';
import { useWorkspaces } from '@/features/sessions/queries';
import { useOwnerTaskPoll, useTaskList } from '@/features/tasks/queries';
import { useConnection } from '@/shell/ConnectionBanner';
import { buildExplorer, type ExplorerNode, type Target } from './model';

/** `lastVisit` is the shell's baseline for "Since you were here"; without it the group is left out. */
export function useExplorer(lastVisit: number | null = null): {
  nodes: ExplorerNode[];
  ready: boolean;
  failed: boolean;
} {
  // The tree is the owner's view of attention, so it polls while it is on screen.
  useAttentionPoll();
  // Owner tasks poll the same way, so a task that stops needing the owner leaves the tree on its own.
  useOwnerTaskPoll(true);
  const attention = useAttention();
  const tasks = useTaskList();
  const workspaces = useWorkspaces();
  const projects = useProjects();
  const schedules = useSchedules();
  const online = useConnection().status !== 'offline';

  const nodes = useMemo(
    () =>
      buildExplorer({
        tasks: tasks.data ?? [],
        attention: attentionItems(attention.data),
        sessions: attention.data?.sessions ?? [],
        workspaces: workspaces.data ?? [],
        projects: keepValid(projects.data, projectRowSchema),
        schedules: schedules.data?.pages.flatMap((page) => page.items) ?? [],
        online,
        lastVisit
      }),
    [tasks.data, attention.data, workspaces.data, projects.data, schedules.data, online, lastVisit]
  );

  return {
    nodes,
    ready: tasks.data !== undefined && attention.data !== undefined,
    failed: (tasks.isError && tasks.data === undefined) || (attention.isError && !attention.data)
  };
}

/** Navigation helpers for the tree. Each returns a function, so the tree can call it on a key or click. */
export function useOpenTarget() {
  const navigate = useNavigate();
  return (target: Target) => {
    // Each move starts the search afresh (the root keeps `?view=`), so the explorer tab is left and a
    // phone shows the page in the chat pane.
    switch (target.to) {
      case 'chat':
        void navigate({ to: '/', search: {} });
        return;
      case 'needs':
        void navigate({ to: '/inbox', search: { item: target.item } });
        return;
      case 'problems':
        void navigate({ to: '/inbox', search: { show: 'problems', item: target.item } });
        return;
      case 'task':
        void navigate({ to: '/tasks/$id', params: { id: target.id }, search: {} });
        return;
      case 'session':
        void navigate({ to: '/sessions/$id', params: { id: target.id }, search: {} });
        return;
      case 'workspace':
        void navigate({ to: '/workspaces/$id', params: { id: target.id }, search: {} });
        return;
      case 'project':
        void navigate({ to: '/projects/$id', params: { id: target.id }, search: {} });
        return;
      case 'schedule':
        void navigate({ to: '/schedules/$id', params: { id: target.id }, search: {} });
        return;
      case 'health':
        void navigate({ to: '/health', search: {} });
        return;
    }
  };
}

/** Puts `text` in the main composer, unsent, and leaves the explorer tab. */
export function useAsk() {
  const navigate = useNavigate();
  return (text: string) =>
    void navigate({
      to: '/',
      search: { prefill: text }
    });
}
