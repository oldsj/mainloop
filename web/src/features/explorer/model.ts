/**
 * The rules for one row: what it says, where it leads and what "Ask Mainloop" prefills. The agent
 * column and the command palette build their lists from these. Pure, so the rules test without a
 * QueryClient. Every fact comes from the existing reads; nothing here decides what a task or session
 * is allowed to do.
 */
import { needsYouItems, type AttentionItem } from '@/features/inbox/model';
import { isHeld, isIdle, statusLine, workGroup } from '@/features/home/model';
import type { ProjectRow } from '@/features/projects/model';
import type { ScheduleView } from '@/features/schedules/model';
import { sessionTone, workspaceTone, type WorkspaceBadgeFields } from '@/features/sessions/model';
import type { Session, TaskView } from '@/lib/contract';
import { childrenOf, statusTone, type Tone } from '@/lib/taskState';
import { isUnseen, type SeenState } from './seen';

export { isHeld };

/** Where a row leads. The shell turns it into a route; a list never builds URLs itself. */
export type Target =
  | { to: 'chat' }
  | { to: 'needs'; item?: string }
  | { to: 'problems'; item?: string }
  | { to: 'task'; id: string }
  | { to: 'session'; id: string }
  | { to: 'workspace'; id: string }
  | { to: 'project'; id: string }
  | { to: 'schedule'; id: string }
  | { to: 'health' }
  /** The Settings section that imports a project. */
  | { to: 'import' };

export interface ExplorerNode {
  /** The list's own prefix plus this row's id, so the same task in two lists has two ids. */
  id: string;
  label: string;
  /** Muted text about the row: where a task stands, a session's status or a workspace's state. */
  detail?: string;
  /** The short name of the row's project (`widgets` for `acme/widgets`), first on the line below. */
  project?: string;
  /** When the row last changed, as an ISO time. The line below ends with its age. */
  at?: string;
  tone?: Tone;
  target?: Target;
  /** The prefill "Ask Mainloop" puts in the composer for this row. */
  ask?: string;
}

export interface ExplorerSource {
  tasks: TaskView[];
  /** Unresolved owner actions from `/attention`. */
  attention: AttentionItem[];
  sessions: Session[];
  workspaces: WorkspaceBadgeFields[];
  projects: ProjectRow[];
  schedules: ScheduleView[];
  /** The clock the finished window is measured from, in ms. Defaults to now. */
  now?: number;
  /** Which finished tasks the owner has looked at in this browser. Without it nothing is unseen. */
  seen?: SeenState | null;
}

/** How long a finished task the owner has not looked at stays listed: seven days. */
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

/**
 * "until 15:40" for later today, "until Sat 15:40" for another day. A hold lasts at most seven
 * days, so the weekday alone says which day, and the line keeps room for the reason.
 */
export function heldUntilText(iso: string, now: number): string {
  const until = new Date(iso);
  const time = until.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  if (until.toDateString() === new Date(now).toDateString()) return `until ${time}`;
  return `until ${until.toLocaleDateString([], { weekday: 'short' })} ${time}`;
}

/**
 * A task the main thread set aside: at rest, with when it comes back and, when the main thread gave
 * one, why it was set aside. Its age is left out: the time that matters is the one it returns at.
 */
function snoozedNode(
  view: TaskView,
  source: ExplorerSource,
  parent: string,
  now: number
): ExplorerNode {
  const node = taskNode(view, source, parent);
  const until = heldUntilText(view.task.held_until ?? '', now);
  const reason = view.task.held_reason?.trim();
  return {
    ...node,
    detail: reason ? `${until} · ${briefSnippet(reason, 80)}` : until,
    at: undefined,
    tone: 'muted'
  };
}

/** "PR #12", or "PR" when the number is not known. */
const prLabel = (view: TaskView) =>
  view.projection.pr_number != null ? `PR #${view.projection.pr_number}` : 'PR';

/** The line under a task whose pull request is ready or landing: the pull request and where it stands. */
function prStageDetail(view: TaskView, stage: PrStage): string {
  if (stage === 'review') return `${prLabel(view)} · CI passing`;
  return view.projection.pr_state === 'merged'
    ? `${prLabel(view)} · merged, settling`
    : `${prLabel(view)} · merging`;
}

/**
 * A task whose pull request is ready for review or landing. Ready for review is finished work the
 * owner has not looked at, so it is teal until its page has been opened and then at rest; a landing
 * one is still moving.
 */
function prStageNode(
  view: TaskView,
  source: ExplorerSource,
  stage: PrStage,
  parent: string
): ExplorerNode {
  const node = taskNode(view, source, parent);
  const tone: Tone =
    stage === 'landing'
      ? 'active'
      : isUnseen(source.seen, view.task.id, view.task.updated_at)
        ? 'unseen'
        : 'muted';
  return { ...node, detail: prStageDetail(view, stage), tone };
}

/**
 * A task as one row wherever it is listed on its own, with the line that says where it stands: when
 * it comes back for a task set aside, its pull request for one that is ready to be looked at
 * (whether or not that already counts as needing the owner) or is landing, and otherwise its status
 * or its children's progress.
 */
export function stagedTaskNode(
  view: TaskView,
  source: ExplorerSource,
  parent: string,
  now: number
): ExplorerNode {
  const group = workGroup(view, null, now);
  const moving = group === 'working' || group === 'needs-you' || group === 'blocked';
  if (moving && isHeld(view, now)) return snoozedNode(view, source, parent, now);
  if (group === 'working') {
    const stage = prStage(view);
    if (stage) return prStageNode(view, source, stage, parent);
  }
  const node = taskNode(view, source, parent);
  // A pull request that only the owner can move on says so in the pull request's own words.
  if (group === 'needs-you' && view.task.reason === 'publication' && prStage(view) === 'review') {
    return { ...node, detail: prStageDetail(view, 'review') };
  }
  return node;
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
 * decision, so it is at rest (hollow), as is a standing supervisor waiting for its next message. A completed task is teal until the owner has opened it,
 * then at rest like any other finished row.
 */
function taskTone(
  view: TaskView,
  group: ReturnType<typeof workGroup>,
  source: ExplorerSource
): Tone {
  if (group === 'needs-you') return 'attention';
  if (group === 'problem' || isIdle(view)) return 'muted';
  const tone = statusTone(view.task.status);
  if (tone !== 'ok') return tone;
  return isUnseen(source.seen, view.task.id, view.task.updated_at) ? 'unseen' : 'muted';
}

/** A dot says a row's state, so a row whose state is merely fine is at rest, not green. */
const atRest = (tone: Tone): Tone => (tone === 'ok' ? 'muted' : tone);

export function taskNode(view: TaskView, source: ExplorerSource, parent: string): ExplorerNode {
  const { task } = view;
  const id = `${parent}/task:${task.id}`;
  const group = workGroup(view, null, source.now);
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
    ask: askPrefill({ label }, target)
  };
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
    ask: askPrefill({ label: session.title || 'session' }, target)
  };
}

export function workspaceNode(workspace: WorkspaceBadgeFields, parent: string): ExplorerNode {
  const target: Target = { to: 'workspace', id: workspace.workspace_id };
  return {
    id: `${parent}workspace:${workspace.workspace_id}`,
    label: `Workspace ${shortId(workspace.workspace_id)}`,
    detail: workspace.observed_state.replaceAll('_', ' '),
    tone: workspaceTone(workspace.observed_state),
    target
  };
}

/** Mainloop's own state: waiting when the owner is needed, working while a task runs, else idle. */
export function mainloopState(needs: number, working: number): { label: string; tone: Tone } {
  if (needs > 0) return { label: 'Needs you', tone: 'attention' };
  if (working > 0) return { label: 'Working', tone: 'active' };
  return { label: 'Idle', tone: 'muted' };
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
