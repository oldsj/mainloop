/**
 * The explorer tree's rules: which rows sit under which top-level node, what each row says, where
 * it leads and what "Ask Mainloop" prefills. Pure, so the rules test without a QueryClient. Every
 * fact comes from the existing reads; nothing here decides what a task or session is allowed to do.
 */
import type { AttentionItem } from '@/features/inbox/model';
import { statusLine, workGroup } from '@/features/home/model';
import type { ProjectRow } from '@/features/projects/model';
import type { ScheduleView } from '@/features/schedules/model';
import { sessionTone, workspaceTone, type WorkspaceBadgeFields } from '@/features/sessions/model';
import type { Session, TaskView } from '@/lib/contract';
import { currentAttempt, statusTone, type Tone } from '@/lib/taskState';
import { isCurrent } from '@/lib/sessionStatus';

/** Where a row leads. The shell turns it into a route; the tree never builds URLs itself. */
export type Target =
  | { to: 'chat'; hitl?: string }
  | { to: 'task'; id: string }
  | { to: 'session'; id: string }
  | { to: 'workspace'; id: string }
  | { to: 'project'; id: string }
  | { to: 'schedule'; id: string }
  | { to: 'health' };

export interface ExplorerNode {
  /** Unique within the tree: the parent's id plus this row's own id, so one task under two projects keeps its own expansion. */
  id: string;
  label: string;
  /** Muted text after the label: a status line, a schedule or a workspace state. */
  detail?: string;
  tone?: Tone;
  /** The muted count on the right of a group row. */
  count?: number;
  target?: Target;
  /** A placeholder that says a group is empty. It is read, never opened. */
  note?: boolean;
  /** The prefill "Ask Mainloop" puts in the composer for this row. */
  ask?: string;
  children: ExplorerNode[];
}

export interface ExplorerSource {
  tasks: TaskView[];
  /** Unresolved owner actions from `/attention`. */
  attention: AttentionItem[];
  sessions: Session[];
  workspaces: WorkspaceBadgeFields[];
  projects: ProjectRow[];
  schedules: ScheduleView[];
  /** Whether the backend answered the last read. Drives the health row. */
  online: boolean;
}

/** How many finished tasks the collapsed "Finished recently" group lists. */
export const FINISHED_LIMIT = 10;

/** The short form of an ID, as the rest of the app shows it. */
export const shortId = (id: string) => id.slice(0, 8);

const byUpdatedDesc = (a: TaskView, b: TaskView) =>
  Date.parse(b.task.updated_at) - Date.parse(a.task.updated_at);

/** Node ids a group starts expanded with. Everything else starts folded. */
export const DEFAULT_EXPANDED: ReadonlySet<string> = new Set(['mainloop', 'needs']);

export function askPrefill(node: Pick<ExplorerNode, 'label'>, target: Target): string | undefined {
  switch (target.to) {
    case 'task':
      return `About task ${node.label} (${shortId(target.id)}): `;
    case 'session':
      return `About session ${node.label} (${shortId(target.id)}): `;
    case 'project':
      return `In project ${node.label}: `;
    case 'schedule':
      return `About schedule ${node.label}: `;
    default:
      return undefined;
  }
}

/** The task's own sessions and workspace, read from its current attempt only. */
function taskChildren(view: TaskView, source: ExplorerSource, path: string): ExplorerNode[] {
  const attempt = currentAttempt(view);
  if (!attempt) return [];
  const children: ExplorerNode[] = [];
  const session = source.sessions.find((row) => row.id === attempt.session_id);
  if (session) children.push(sessionNode(session, `${path}/`));
  const workspace = source.workspaces.find((row) => row.workspace_id === attempt.workspace_id);
  if (workspace) children.push(workspaceNode(workspace, `${path}/`));
  return children;
}

export function taskNode(view: TaskView, source: ExplorerSource, parent: string): ExplorerNode {
  const { task } = view;
  const id = `${parent}/task:${task.id}`;
  const needsYou = workGroup(view, null) === 'needs-you';
  const target: Target = { to: 'task', id: task.id };
  return {
    id,
    label: task.title,
    detail: statusLine(view),
    tone: needsYou ? 'attention' : statusTone(task.status),
    target,
    ask: askPrefill({ label: task.title }, target),
    children: taskChildren(view, source, id)
  };
}

export function sessionNode(session: Session, parent: string): ExplorerNode {
  const id = `${parent}session:${session.id}`;
  const target: Target = { to: 'session', id: session.id };
  return {
    id,
    label: session.title || 'Untitled session',
    detail: session.needs_owner ? 'Needs you' : session.status.replaceAll('_', ' '),
    tone: sessionTone(session),
    target,
    ask: askPrefill({ label: session.title || 'session' }, target),
    children: []
  };
}

export function workspaceNode(workspace: WorkspaceBadgeFields, parent: string): ExplorerNode {
  const target: Target = { to: 'workspace', id: workspace.workspace_id };
  return {
    id: `${parent}workspace:${workspace.workspace_id}`,
    label: `Workspace ${shortId(workspace.workspace_id)}`,
    detail: workspace.observed_state.replaceAll('_', ' '),
    tone: workspaceTone(workspace.observed_state),
    target,
    children: []
  };
}

function attentionNode(item: AttentionItem, parent: string): ExplorerNode {
  const hitl = item.queue_item?.hitl_request_id ?? undefined;
  const target: Target = item.task_id ? { to: 'task', id: item.task_id } : { to: 'chat', hitl };
  return {
    id: `${parent}/attention:${item.id}`,
    label: item.title,
    detail: item.needed,
    tone: 'attention',
    target,
    children: []
  };
}

/** A row that says the group is empty. It has no target, so it is only read, not opened. */
const note = (id: string, label: string): ExplorerNode => ({ id, label, note: true, children: [] });

/** Mainloop's own state: waiting when the owner is needed, working while a task runs, else idle. */
export function mainloopState(needs: number, working: number): { label: string; tone: Tone } {
  if (needs > 0) return { label: 'Waiting on you', tone: 'attention' };
  if (working > 0) return { label: 'Working', tone: 'active' };
  return { label: 'Idle', tone: 'muted' };
}

/**
 * The top-level tree, in the order the owner sees it: Mainloop, Needs you, Working, Projects,
 * Schedules, Finished recently, Health.
 */
export function buildExplorer(source: ExplorerSource): ExplorerNode[] {
  const groups = {
    needs: [] as ExplorerNode[],
    working: [] as TaskView[],
    finished: [] as TaskView[]
  };
  const attentionTasks = new Set<string>();
  for (const item of source.attention) {
    if (item.task_id) attentionTasks.add(item.task_id);
  }

  // Needs you: owner actions first, then failed or blocked tasks that no action already names.
  const needs = source.attention.map((item) => attentionNode(item, 'needs'));
  for (const view of [...source.tasks].sort(byUpdatedDesc)) {
    const group = workGroup(view, null);
    if (group === 'working') groups.working.push(view);
    if ((group === 'needs-you' || group === 'blocked') && !attentionTasks.has(view.task.id)) {
      needs.push(taskNode(view, source, 'needs'));
    }
    if (view.task.status === 'completed' || view.task.status === 'cancelled') {
      groups.finished.push(view);
    }
  }
  groups.needs = needs;

  const live = source.sessions.filter((session) => isCurrent(session));
  const mainloop = mainloopState(needs.length, groups.working.length);

  const projects = source.projects.map((project) => {
    const path = `projects/${project.id}`;
    const tasks = source.tasks
      .filter((view) => view.task.project_id === project.id)
      .sort(byUpdatedDesc);
    const target: Target = { to: 'project', id: project.id };
    return {
      id: path,
      label: project.full_name,
      count: tasks.length,
      target,
      ask: askPrefill({ label: project.full_name }, target),
      children: tasks.map((view) => taskNode(view, source, path))
    } satisfies ExplorerNode;
  });

  const schedules = source.schedules.map((schedule) => {
    const target: Target = { to: 'schedule', id: schedule.id };
    return {
      id: `schedules/${schedule.id}`,
      label: schedule.config.name,
      detail: schedule.config.paused ? 'Paused' : schedule.config.schedule,
      tone: schedule.config.paused ? 'muted' : 'ok',
      target,
      ask: askPrefill({ label: schedule.config.name }, target),
      children: []
    } satisfies ExplorerNode;
  });

  const finished = groups.finished.sort(byUpdatedDesc);

  return [
    {
      id: 'mainloop',
      label: 'Mainloop',
      detail: mainloop.label,
      tone: mainloop.tone,
      target: { to: 'chat' },
      children: live.map((session) => sessionNode(session, 'mainloop/'))
    },
    {
      id: 'needs',
      label: 'Needs you',
      count: needs.length,
      tone: needs.length > 0 ? 'attention' : undefined,
      children: needs.length > 0 ? needs : [note('needs/none', 'Nothing needs you')]
    },
    {
      id: 'working',
      label: 'Working',
      count: groups.working.length,
      children:
        groups.working.length > 0
          ? groups.working.map((view) => taskNode(view, source, 'working'))
          : [note('working/none', 'Nothing running')]
    },
    {
      id: 'projects',
      label: 'Projects',
      count: projects.length,
      children: projects.length > 0 ? projects : [note('projects/none', 'No projects yet')]
    },
    {
      id: 'schedules',
      label: 'Schedules',
      count: schedules.length,
      children: schedules.length > 0 ? schedules : [note('schedules/none', 'No schedules')]
    },
    {
      id: 'finished',
      label: 'Finished recently',
      count: finished.length,
      children: finished.slice(0, FINISHED_LIMIT).map((view) => taskNode(view, source, 'finished'))
    },
    {
      id: 'health',
      label: 'Health',
      detail: source.online ? 'Connected' : 'Offline',
      tone: source.online ? 'ok' : 'bad',
      target: { to: 'health' },
      children: []
    }
  ];
}

/** The count the Explorer tab's badge shows: the Needs you group. */
export function needsCount(nodes: ExplorerNode[]): number {
  return nodes.find((node) => node.id === 'needs')?.count ?? 0;
}

/** The visible rows in order, with each row's depth and parent. Drives keyboard movement. */
export interface VisibleRow {
  node: ExplorerNode;
  depth: number;
  parentId: string | null;
}

export function visibleRows(
  nodes: ExplorerNode[],
  isExpanded: (id: string) => boolean,
  depth = 1,
  parentId: string | null = null
): VisibleRow[] {
  const rows: VisibleRow[] = [];
  for (const node of nodes) {
    rows.push({ node, depth, parentId });
    if (node.children.length > 0 && isExpanded(node.id)) {
      rows.push(...visibleRows(node.children, isExpanded, depth + 1, node.id));
    }
  }
  return rows;
}
