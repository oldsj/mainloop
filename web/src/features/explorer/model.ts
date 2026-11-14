/**
 * The explorer tree's rules: which rows sit under which top-level node, what each row says, where
 * it leads and what "Ask Mainloop" prefills. Pure, so the rules test without a QueryClient. Every
 * fact comes from the existing reads; nothing here decides what a task or session is allowed to do.
 */
import {
  attentionKey,
  needsYouItems,
  problemItems,
  type AttentionItem
} from '@/features/inbox/model';
import {
  countText,
  sinceCounts,
  statusLine,
  workGroup,
  type SinceCounts
} from '@/features/home/model';
import type { ProjectRow } from '@/features/projects/model';
import type { ScheduleView } from '@/features/schedules/model';
import { sessionTone, workspaceTone, type WorkspaceBadgeFields } from '@/features/sessions/model';
import { hasAvailableAction } from '@/features/tasks/cache';
import type { Session, TaskView } from '@/lib/contract';
import { childrenOf, currentAttempt, statusTone, type Tone } from '@/lib/taskState';
import { isCurrent } from '@/lib/sessionStatus';
import { isUnseen, type SeenState } from './seen';

/** Where a row leads. The shell turns it into a route; the tree never builds URLs itself. */
export type Target =
  | { to: 'chat' }
  | { to: 'needs'; item?: string }
  | { to: 'problems'; item?: string }
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
  /**
   * Muted text about the row: a status line, what an owner action needs, a schedule or a workspace
   * state. A top-level row shows it beside the label; any other row shows it on the line below.
   */
  detail?: string;
  /** The short name of the row's project (`widgets` for `acme/widgets`), first on the line below. */
  project?: string;
  /** When the row last changed, as an ISO time. The line below ends with its age. */
  at?: string;
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
  /** Whether the backend answered the last read. Drives the Insights row. */
  online: boolean;
  /** The last app open, in ms, for "Since you were here". Null on a first visit or with storage blocked. */
  lastVisit?: number | null;
  /** The clock the finished window is measured from, in ms. Defaults to now. */
  now?: number;
  /** Which finished tasks the owner has looked at in this browser. Without it nothing is unseen. */
  seen?: SeenState | null;
}

/** How far back "Finished recently" reaches: tasks completed or cancelled in the last seven days. */
export const FINISHED_WINDOW_MS = 7 * 24 * 60 * 60 * 1000;

/** The short form of an ID, as the rest of the app shows it. */
export const shortId = (id: string) => id.slice(0, 8);

/** A task with no stored title, which the schema has never allowed but a row still guards against. */
const FALLBACK_TITLE = 'Untitled task';

/** A project's short name: the repository without its owner. The row's title says the rest. */
export function shortProject(fullName: string): string {
  return fullName.split('/').pop() || fullName;
}

/** The first `max` characters of a brief or report, flattened to one line. */
export function briefSnippet(brief: string, max = 140): string {
  const flat = brief.trim().replace(/\s+/g, ' ');
  return flat.length > max ? `${flat.slice(0, max).trimEnd()}…` : flat;
}

const byUpdatedDesc = (a: TaskView, b: TaskView) =>
  Date.parse(b.task.updated_at) - Date.parse(a.task.updated_at);

/**
 * Node ids a group starts expanded with. Everything else starts folded. Ready for review is open:
 * it is the owner's own next step.
 */
export const DEFAULT_EXPANDED: ReadonlySet<string> = new Set([
  'mainloop',
  'needs',
  'since',
  'review'
]);

/**
 * Where a working task's pull request stands, from the facts the task already carries (no new
 * read). The agent's part is over in both; what is left is the review or the merge.
 *
 * - `review`: the pull request is open, CI passed on its head, and no merge has been proposed. It
 *   is waiting to be looked at.
 * - `landing`: a merge is under way (the merge service holds a proposal for it), or the pull
 *   request has merged and the task is settling.
 *
 * CI that is failing, still running or not known for this head is neither: that task is still work
 * in progress. The server already reports CI as unknown when its last look is stale.
 */
export type PrStage = 'review' | 'landing';

export function prStage(view: TaskView): PrStage | null {
  const p = view.projection;
  if (p.pr_state === 'merged') return 'landing';
  if (p.pr_state !== 'open') return null;
  if (p.merge_proposal_id) return 'landing';
  return p.ci_state === 'success' ? 'review' : null;
}

/** "PR #12", or "PR" when the number is not known. */
const prLabel = (view: TaskView) =>
  view.projection.pr_number != null ? `PR #${view.projection.pr_number}` : 'PR';

/** The line under a row in Ready for review or Landing: the pull request and where it stands. */
function prStageDetail(view: TaskView, stage: PrStage): string {
  if (stage === 'review') return `${prLabel(view)} · CI passing`;
  return view.projection.pr_state === 'merged'
    ? `${prLabel(view)} · merged, settling`
    : `${prLabel(view)} · merging`;
}

/**
 * A row in Ready for review or Landing. Ready for review is finished work the owner has not looked
 * at, so it is teal until its page has been opened and then at rest; Landing is still moving.
 */
function prStageNode(view: TaskView, source: ExplorerSource, stage: PrStage): ExplorerNode {
  const node = taskNode(view, source, stage);
  const tone: Tone =
    stage === 'landing'
      ? 'active'
      : isUnseen(source.seen, view.task.id, view.task.updated_at)
        ? 'unseen'
        : 'muted';
  return { ...node, detail: prStageDetail(view, stage), tone };
}

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

/** The task's own child tasks, then its session and workspace, read from its current attempt only. */
function taskChildren(
  view: TaskView,
  source: ExplorerSource,
  path: string,
  childTasks: TaskView[]
): ExplorerNode[] {
  const children: ExplorerNode[] = childTasks.map((child) => taskNode(child, source, path));
  const attempt = currentAttempt(view);
  if (!attempt) return children;
  const session = source.sessions.find((row) => row.id === attempt.session_id);
  if (session) children.push(sessionNode(session, `${path}/`));
  const workspace = source.workspaces.find((row) => row.workspace_id === attempt.workspace_id);
  if (workspace) children.push(workspaceNode(workspace, `${path}/`));
  return children;
}

/** Whether a direct child needs the owner, is still moving, is blocked, or is done. */
function childBucket(
  child: TaskView,
  attentionTaskIds: ReadonlySet<string>
): 'waiting' | 'blocked' | 'done' | 'running' {
  if (attentionTaskIds.has(child.task.id)) return 'waiting';
  const { status } = child.task;
  if (status === 'completed' || status === 'cancelled') return 'done';
  if (status === 'blocked' || status === 'failed') return 'blocked';
  return 'running';
}

/** "needs you" with its verb agreeing: "1 needs you", "2 need you". The label for red, everywhere. */
const needsYou = (count: number) => (count === 1 ? 'needs you' : 'need you');

/** "3/5 done · 1 running · 1 blocked · 1 needs you", each part left out when it is zero. */
function childProgressText(childTasks: TaskView[], attention: AttentionItem[]): string {
  // A problem item is Mainloop's own recovery work, not an owner decision, so a child behind one
  // reads as blocked, not waiting on the owner.
  const attentionTaskIds = new Set(
    needsYouItems(attention).flatMap((item) => (item.task_id ? [item.task_id] : []))
  );
  const counts = { done: 0, running: 0, blocked: 0, waiting: 0 };
  for (const child of childTasks) counts[childBucket(child, attentionTaskIds)]++;
  const parts = [`${counts.done}/${childTasks.length} done`];
  if (counts.running > 0) parts.push(`${counts.running} running`);
  if (counts.blocked > 0) parts.push(`${counts.blocked} blocked`);
  if (counts.waiting > 0) parts.push(`${counts.waiting} ${needsYou(counts.waiting)}`);
  return parts.join(' · ');
}

/** The task's own latest report, or else its most recently updated child's latest report. */
function latestReportLine(view: TaskView, childTasks: TaskView[]): string | undefined {
  const ownReport = view.reports.at(-1);
  if (ownReport) return briefSnippet(ownReport.summary);
  for (const child of [...childTasks].sort(byUpdatedDesc)) {
    const report = child.reports.at(-1);
    if (report) return briefSnippet(report.summary);
  }
  return undefined;
}

/** A parent's detail: its children's progress, then the latest report line when there is one. */
function parentDetail(view: TaskView, childTasks: TaskView[], source: ExplorerSource): string {
  const progress = childProgressText(childTasks, source.attention);
  const report = latestReportLine(view, childTasks);
  return report ? `${progress} · ${report}` : progress;
}

/**
 * A task row's dot. Needs you is red. A problem is Mainloop's own recovery work, not an owner
 * decision, so it is at rest (hollow). A completed task is teal until the owner has opened it,
 * then at rest like any other finished row.
 */
function taskTone(
  view: TaskView,
  group: ReturnType<typeof workGroup>,
  source: ExplorerSource
): Tone {
  if (group === 'needs-you') return 'attention';
  if (group === 'problem') return 'muted';
  const tone = statusTone(view.task.status);
  if (tone !== 'ok') return tone;
  return isUnseen(source.seen, view.task.id, view.task.updated_at) ? 'unseen' : 'muted';
}

/** A dot says a row's state, so a row whose state is merely fine is at rest, not green. */
const atRest = (tone: Tone): Tone => (tone === 'ok' ? 'muted' : tone);

export function taskNode(view: TaskView, source: ExplorerSource, parent: string): ExplorerNode {
  const { task } = view;
  const id = `${parent}/task:${task.id}`;
  const group = workGroup(view, null);
  const target: Target = { to: 'task', id: task.id };
  const title = task.title.trim();
  const label = title || briefSnippet(task.brief, 60) || FALLBACK_TITLE;
  const project = source.projects.find((row) => row.id === task.project_id);
  const childTasks = childrenOf(source.tasks, task.id);
  return {
    id,
    label,
    detail: childTasks.length > 0 ? parentDetail(view, childTasks, source) : statusLine(view),
    project: project ? shortProject(project.full_name) : undefined,
    at: task.updated_at,
    tone: taskTone(view, group, source),
    target,
    ask: askPrefill({ label }, target),
    children: taskChildren(view, source, id, childTasks)
  };
}

/** A task nested under a loaded parent is shown there, not again as a flat sibling. */
function hasLoadedParent(view: TaskView, source: ExplorerSource): boolean {
  const parentId = view.task.parent_task_id;
  return parentId != null && source.tasks.some((candidate) => candidate.task.id === parentId);
}

export function sessionNode(session: Session, parent: string): ExplorerNode {
  const id = `${parent}session:${session.id}`;
  const target: Target = { to: 'session', id: session.id };
  return {
    id,
    label: session.title || 'Untitled session',
    detail: session.needs_owner ? 'Needs you' : session.status.replaceAll('_', ' '),
    tone: atRest(sessionTone(session)),
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

/** What an owner action is called in an Ask Mainloop prefill. */
const KIND: Record<string, string> = {
  approval: 'approval',
  question: 'question',
  owner_decision: 'decision'
};

/**
 * An owner action. It opens the answer view on its own row, so a question, plan review or approval
 * is answered there whether or not it belongs to a task.
 */
function attentionNode(item: AttentionItem, parent: string): ExplorerNode {
  const target: Target = { to: 'needs', item: attentionKey(item) };
  return {
    id: `${parent}/attention:${item.id}`,
    label: item.title,
    detail: item.needed,
    tone: 'attention',
    target,
    ask: item.task_id
      ? `About ${KIND[item.kind] ?? 'request'} ${item.title} (task ${shortId(item.task_id)}): `
      : `About ${KIND[item.kind] ?? 'request'} ${item.title}: `,
    children: []
  };
}

/**
 * A problem: Mainloop's own recovery work (a stall, an unconfirmed stop), with no decision yet for
 * the owner. It opens the Problems view, not Needs you, and reads muted rather than attention, so
 * only a real owner decision shows yellow.
 */
function problemNode(item: AttentionItem, parent: string): ExplorerNode {
  const target: Target = { to: 'problems', item: attentionKey(item) };
  return {
    id: `${parent}/problem:${item.id}`,
    label: item.title,
    detail: item.needed,
    tone: 'muted',
    target,
    children: []
  };
}

/**
 * The "Since you were here" rows. The waiting row opens the answer view, on the latest task's action
 * when one is listed, as the same work does under Needs you; the others open the latest task.
 */
const SINCE: { key: keyof SinceCounts; label: (count: number) => string; tone: Tone }[] = [
  { key: 'waiting', label: needsYou, tone: 'attention' },
  { key: 'failed', label: () => 'failed', tone: 'bad' },
  // New since the owner was last here, so it is finished and not yet seen.
  { key: 'merged', label: () => 'merged', tone: 'unseen' }
];

/**
 * What changed since the last app open, as a group. It is left out when nothing changed or there is
 * no baseline, so it never shows empty.
 */
function sinceNode(source: ExplorerSource): ExplorerNode[] {
  const counts = sinceCounts(source.tasks, source.lastVisit ?? null);
  const children = SINCE.flatMap(({ key, label, tone }): ExplorerNode[] => {
    const [latest] = counts[key];
    if (!latest) return [];
    return [
      {
        id: `since/${key}`,
        label: `${countText(counts[key].length, 'task')} ${label(counts[key].length)}`,
        detail: latest.task.title,
        tone,
        target:
          key === 'waiting'
            ? needsTarget(source, latest.task.id)
            : { to: 'task', id: latest.task.id },
        children: []
      }
    ];
  });
  if (children.length === 0) return [];
  // A task can be counted twice (failed and waiting on the owner), so the group counts distinct tasks.
  const count = new Set(SINCE.flatMap(({ key }) => counts[key].map((view) => view.task.id))).size;
  return [{ id: 'since', label: 'Since you were here', count, children }];
}

/** The answer view, on the task's first listed owner action if it has one. */
function needsTarget(source: ExplorerSource, taskId: string): Target {
  const item = source.attention.find((entry) => entry.task_id === taskId);
  return item ? { to: 'needs', item: attentionKey(item) } : { to: 'needs' };
}

/** A row that says the group is empty. It has no target, so it is only read, not opened. */
const note = (id: string, label: string): ExplorerNode => ({ id, label, note: true, children: [] });

/** Mainloop's own state: waiting when the owner is needed, working while a task runs, else idle. */
export function mainloopState(needs: number, working: number): { label: string; tone: Tone } {
  if (needs > 0) return { label: 'Needs you', tone: 'attention' };
  if (working > 0) return { label: 'Working', tone: 'active' };
  return { label: 'Idle', tone: 'muted' };
}

/**
 * The top-level tree, in the order the owner sees it: Mainloop, Needs you, Problems, Since you were
 * here (only when something changed), Working, Ready for review and Landing (each only when it has
 * rows), Projects, Schedules, Finished recently, Insights (the health row; its page is still
 * `/health`).
 */
export function buildExplorer(source: ExplorerSource): ExplorerNode[] {
  const groups = {
    needs: [] as ExplorerNode[],
    problems: [] as ExplorerNode[],
    working: [] as TaskView[],
    review: [] as TaskView[],
    landing: [] as TaskView[],
    finished: [] as TaskView[]
  };
  const attentionTasks = new Set<string>();
  for (const item of source.attention) {
    if (item.task_id) attentionTasks.add(item.task_id);
  }

  // Needs you: owner actions first, then a blocked task (not a problem) that no action already
  // names. A failed or blocked task with nothing the owner can do about it (every action
  // unavailable) is not an owner decision either: workGroup leaves it out of both groups below, so
  // it falls through to Finished recently or its project instead of piling up here forever.
  const needs = needsYouItems(source.attention).map((item) => attentionNode(item, 'needs'));
  // Problems: Mainloop's own recovery work first, then blocked tasks no problem item already names.
  // Kept apart from Needs you: a problem is not an owner decision, so it never shows there.
  const problems = problemItems(source.attention).map((item) => problemNode(item, 'problems'));
  const now = source.now ?? Date.now();
  const finishedSince = now - FINISHED_WINDOW_MS;
  for (const view of [...source.tasks].sort(byUpdatedDesc)) {
    const group = workGroup(view, null);
    const named = attentionTasks.has(view.task.id);
    const nested = hasLoadedParent(view, source);
    // A task nested under its parent elsewhere in the tree is not also a flat Working or
    // Finished row; Needs you stays flat regardless, so an owner action is never hidden a level down.
    // Working is what is still in progress. A task whose pull request is ready to be looked at, or
    // is landing, moves to its own group, so Working does not hide what only waits on a review.
    if (group === 'working' && !named && !nested) {
      const stage = prStage(view);
      if (stage === 'review') groups.review.push(view);
      else if (stage === 'landing') groups.landing.push(view);
      else groups.working.push(view);
    }
    if ((group === 'needs-you' || group === 'blocked') && !named) {
      needs.push(taskNode(view, source, 'needs'));
    }
    if (group === 'problem' && !named) {
      problems.push(taskNode(view, source, 'problems'));
    }
    // A failed task joins completed/cancelled ones here once it has nothing left for the owner to
    // do: same recency window, same fallback to its project once that window passes.
    const finished =
      view.task.status === 'completed' ||
      view.task.status === 'cancelled' ||
      (view.task.status === 'failed' && !hasAvailableAction(view));
    if (finished && Date.parse(view.task.updated_at) >= finishedSince && !nested) {
      groups.finished.push(view);
    }
  }
  groups.needs = needs;
  groups.problems = problems;

  const live = source.sessions.filter((session) => isCurrent(session));
  // A merge under way is still Mainloop working; a pull request waiting to be looked at is not.
  const mainloop = mainloopState(needs.length, groups.working.length + groups.landing.length);

  const projects = source.projects.map((project) => {
    const path = `projects/${project.id}`;
    const tasks = source.tasks
      .filter((view) => view.task.project_id === project.id && !hasLoadedParent(view, source))
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
      tone: 'muted',
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
      target: { to: 'needs' },
      tone: needs.length > 0 ? 'attention' : undefined,
      children: needs.length > 0 ? needs : [note('needs/none', 'Nothing needs you')]
    },
    {
      id: 'problems',
      label: 'Problems',
      count: problems.length,
      target: { to: 'problems' },
      children:
        problems.length > 0 ? problems : [note('problems/none', 'Nothing Mainloop is handling')]
    },
    ...sinceNode(source),
    {
      id: 'working',
      label: 'Working',
      count: groups.working.length,
      children:
        groups.working.length > 0
          ? groups.working.map((view) => taskNode(view, source, 'working'))
          : [note('working/none', 'Nothing running')]
    },
    // Left out when empty, like Since you were here: most of the time nothing is at either stage.
    ...(groups.review.length > 0
      ? [
          {
            id: 'review',
            label: 'Ready for review',
            count: groups.review.length,
            children: groups.review.map((view) => prStageNode(view, source, 'review'))
          } satisfies ExplorerNode
        ]
      : []),
    ...(groups.landing.length > 0
      ? [
          {
            id: 'landing',
            label: 'Landing',
            count: groups.landing.length,
            children: groups.landing.map((view) => prStageNode(view, source, 'landing'))
          } satisfies ExplorerNode
        ]
      : []),
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
      children: finished.map((view) => taskNode(view, source, 'finished'))
    },
    {
      id: 'health',
      label: 'Insights',
      detail: source.online ? 'Connected' : 'Offline',
      tone: source.online ? 'ok' : 'bad',
      target: { to: 'health' },
      children: []
    }
  ];
}

/**
 * The row the current page belongs to, from the router's path and search. It is the inverse of the
 * shell's navigation for a `Target`. A page no row leads to (a new schedule, the activity view)
 * gives null.
 */
export function targetOfLocation(
  pathname: string,
  search: { show?: unknown; item?: unknown } = {}
): Target | null {
  const item = typeof search.item === 'string' && search.item ? search.item : undefined;
  const path = pathname.replace(/\/+$/, '') || '/';
  if (path === '/') return { to: 'chat' };
  if (path === '/health') return { to: 'health' };
  if (path === '/inbox') {
    if (search.show === 'problems') return { to: 'problems', item };
    if (search.show === 'activity') return null;
    return { to: 'needs', item };
  }
  const match = /^\/(tasks|sessions|workspaces|projects|schedules)\/([^/]+)$/.exec(path);
  if (!match || match[2] === 'new') return null;
  const id = decodeURIComponent(match[2]!);
  switch (match[1]) {
    case 'tasks':
      return { to: 'task', id };
    case 'sessions':
      return { to: 'session', id };
    case 'workspaces':
      return { to: 'workspace', id };
    case 'projects':
      return { to: 'project', id };
    default:
      return { to: 'schedule', id };
  }
}

/** Whether two targets lead to the same page: the same kind, and the same id or inbox item. */
export function sameTarget(a: Target | null | undefined, b: Target | null | undefined): boolean {
  if (!a || !b || a.to !== b.to) return false;
  const idOf = (t: Target) => ('id' in t ? t.id : undefined);
  const itemOf = (t: Target) => ('item' in t ? t.item : undefined);
  return idOf(a) === idOf(b) && itemOf(a) === itemOf(b);
}

/**
 * The desktop sidebar's two views. **Sessions** is the work and what it needs: Mainloop, Needs you,
 * Problems, Since you were here, Working, Ready for review, Landing and Finished recently. **Projects** is the catalogue:
 * Projects and Schedules. Insights is in neither: on desktop the connection is in the status bar.
 */
export type SidebarView = 'sessions' | 'projects';

export const SIDEBAR_VIEWS: readonly { id: SidebarView; label: string }[] = [
  { id: 'sessions', label: 'Sessions' },
  { id: 'projects', label: 'Projects' }
];

const VIEW_NODES: Record<SidebarView, readonly string[]> = {
  sessions: ['mainloop', 'needs', 'problems', 'since', 'working', 'review', 'landing', 'finished'],
  projects: ['projects', 'schedules']
};

/** The top-level nodes one sidebar view shows, in the tree's own order. */
export function viewNodes(nodes: ExplorerNode[], view: SidebarView): ExplorerNode[] {
  return nodes.filter((node) => VIEW_NODES[view].includes(node.id));
}

/** The view a top-level node belongs to, or null for one that is in neither (Insights). */
export function viewOf(nodeId: string): SidebarView | null {
  const top = nodeId.split('/')[0] ?? nodeId;
  return SIDEBAR_VIEWS.find(({ id }) => VIEW_NODES[id].includes(top))?.id ?? null;
}

/** The three counts the status bar shows, read from the same groups the tree draws. */
export function shellCounts(nodes: ExplorerNode[]): {
  needs: number;
  working: number;
  problems: number;
} {
  const count = (id: string) => nodes.find((node) => node.id === id)?.count ?? 0;
  return { needs: count('needs'), working: count('working'), problems: count('problems') };
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
