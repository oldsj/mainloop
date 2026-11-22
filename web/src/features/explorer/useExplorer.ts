/**
 * The explorer's data and its two actions. Reads reuse the queries the pages use, so SSE
 * invalidation reaches the lists without a second map. Opening a row and asking Mainloop both leave
 * the phone's list, so the page or the composer shows in its place.
 */
import { useLocation, useNavigate } from '@tanstack/react-router';
import { useEffect, useMemo } from 'react';
import { attentionItems } from '@/features/inbox/model';
import { needsTotal, ownerWork, problemTotal } from '@/features/inbox/ownerWork';
import { useAttention } from '@/features/inbox/queries';
import { useAttentionPoll } from '@/features/inbox/poll';
import { keepValid, projectRowSchema } from '@/features/projects/model';
import { useProjects } from '@/features/projects/queries';
import { useSchedules } from '@/features/schedules/queries';
import { useWorkspaces } from '@/features/sessions/queries';
import { useOwnerTaskPoll, useTaskList } from '@/features/tasks/queries';
import { currentAttempt } from '@/lib/taskState';
import { buildAgentColumn, spaceOfPage, type AgentColumnModel } from './agents';
import { prStage, targetOfLocation, type ExplorerSource, type Target } from './model';
import { useSeen } from './seen';

/**
 * What the shell keeps up while it is on screen: the two polls, the mark that a task has been looked
 * at, and the two counts every badge shows (what needs the owner, and what Mainloop is handling).
 */
export function useExplorer(): {
  needs: number;
  problems: number;
  ready: boolean;
  failed: boolean;
} {
  // The shell is the owner's view of attention, so it polls while it is on screen.
  useAttentionPoll();
  // Owner tasks poll the same way, so a task that stops needing the owner leaves the lists on its own.
  useOwnerTaskPoll(true);
  const tasks = useTaskList();

  // A finished task is "unseen" until the owner opens its page, or the page of its session, on any
  // device. It is marked when that page opens, and again when it changes under the owner while it
  // is the kind of row that shows seen-ness (completed, or a pull request ready to be looked at).
  // A running task is marked once per status, so watching it does not write on every update.
  const { seen, mark } = useSeen();
  const page = useLocation({
    select: (location) => targetOfLocation(location.pathname, location.search),
    structuralSharing: true
  });
  const openView = useMemo(() => {
    if (page?.to !== 'task' && page?.to !== 'session') return undefined;
    return (tasks.data ?? []).find((view) =>
      page.to === 'task' ? view.task.id === page.id : currentAttempt(view)?.session_id === page.id
    );
  }, [page, tasks.data]);
  const open = openView?.task;
  const openId = open?.id;
  const openChanged = open?.updated_at;
  const shown = open?.status === 'completed' || (openView ? prStage(openView) === 'review' : false);
  const markKey = shown ? openChanged : open?.status;
  useEffect(() => {
    if (openId) mark(openId, openChanged);
    // `openChanged` is read at the time of the mark; `markKey` decides when one is due. `seen` is a
    // dependency so the first mark runs once the owner's record has been read.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [openId, markKey, mark, seen]);

  const { source, ready, failed } = useExplorerSource();
  // The same list the Inbox draws, so the counts here and the rows there always agree.
  const work = useMemo(() => ownerWork(source.attention, source.tasks), [source]);
  return { needs: needsTotal(work), problems: problemTotal(work), ready, failed };
}

/**
 * What the agent column and the command palette are both built from: the reads the pages already
 * make, so a second caller costs no request. It does not poll; the shell's explorer does.
 */
export function useExplorerSource(): {
  source: ExplorerSource;
  ready: boolean;
  failed: boolean;
} {
  const attention = useAttention();
  const tasks = useTaskList();
  const workspaces = useWorkspaces();
  const projects = useProjects();
  const schedules = useSchedules();
  const { seen } = useSeen();
  const source = useMemo(
    (): ExplorerSource => ({
      tasks: tasks.data ?? [],
      attention: attentionItems(attention.data),
      sessions: attention.data?.sessions ?? [],
      workspaces: workspaces.data ?? [],
      projects: keepValid(projects.data, projectRowSchema),
      schedules: schedules.data?.pages.flatMap((page) => page.items) ?? [],
      seen
    }),
    [tasks.data, attention.data, workspaces.data, projects.data, schedules.data, seen]
  );
  return {
    source,
    ready: tasks.data !== undefined && attention.data !== undefined,
    failed: (tasks.isError && tasks.data === undefined) || (attention.isError && !attention.data)
  };
}

/**
 * The left column's spaces and agents, the page being shown and the space it belongs to. Built from
 * the explorer's reads, so it changes with them.
 */
export interface AgentColumnState {
  model: AgentColumnModel;
  /** The page being shown. */
  page: Target | null;
  /** The space that page belongs to. */
  space: string;
}

export function useAgentColumn(): AgentColumnState {
  const { source } = useExplorerSource();
  const page = useLocation({
    select: (location) => targetOfLocation(location.pathname, location.search),
    structuralSharing: true
  });
  const model = useMemo(() => buildAgentColumn(source, page), [source, page]);
  const space = useMemo(() => spaceOfPage(page, source), [page, source]);
  return { model, page, space };
}

/** Navigation helpers for the lists. Each returns a function, so a row can call it on a key or click. */
export function useOpenTarget() {
  const navigate = useNavigate();
  return (target: Target) => {
    // Each move starts the search afresh (the root keeps `?view=`), so the phone's list is put away
    // and the page shows.
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
      case 'import':
        void navigate({ to: '/settings', search: {}, hash: 'import-project' });
        return;
    }
  };
}

/** Puts `text` in the main composer, unsent, and puts the phone's list away. */
export function useAsk() {
  const navigate = useNavigate();
  return (text: string) =>
    void navigate({
      to: '/',
      search: { prefill: text }
    });
}
