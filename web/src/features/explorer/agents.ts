/**
 * The left column's two lists, in the shape of a terminal workspace manager: **spaces** (main, the
 * main thread's own, then each project) and the **agents** working in them (the main thread, each task's session, and
 * a live session that belongs to no task). Pure, and built from the reads the explorer already
 * makes, so nothing here decides what a task or session is allowed to do.
 */
import { needsYouItems, taskIdOf, type AttentionItem } from '@/features/inbox/model';
import { needsTotal, ownerWork } from '@/features/inbox/ownerWork';
import { workGroup } from '@/features/home/model';
import type { Session, TaskView } from '@/lib/contract';
import { isCurrent } from '@/lib/sessionStatus';
import { currentAttempt, isTerminal, taskForSession, type Tone } from '@/lib/taskState';
import {
  FINISHED_WINDOW_MS,
  mainloopState,
  sameTarget,
  sessionNode,
  shortProject,
  stagedTaskNode,
  type ExplorerSource,
  type Target
} from './model';
import { isUnseen } from './seen';

/** The space that is not a project: the orchestrator, one level above every project. */
export const MAINLOOP_SPACE = 'mainloop';

/**
 * What an agent is: the main thread, a coordination task's session (it directs other tasks), a code
 * task's session, or a live session that belongs to no task.
 */
export type AgentRole = 'main' | 'supervisor' | 'worker' | 'session';

export interface AgentRow {
  /** Unique in the list: `main`, `task:<id>` or `session:<id>`. */
  id: string;
  /** The space it works in: a project id, or `MAINLOOP_SPACE`. */
  space: string;
  /** The space's name. The list groups its rows under it. */
  spaceLabel: string;
  /** The row's first line: the task's or session's own title. */
  label: string;
  role: AgentRole;
  /** The project's standing supervisor: one per project, first among its agents, never finished. */
  standing: boolean;
  /** Where it stands or what it needs: the row's second line. */
  detail?: string;
  /** When it last changed, as an ISO time. The second line ends with its age. */
  at?: string;
  tone: Tone;
  /** Mainloop's own recovery work (a stall, a lost session): at rest, but not idle. */
  problem: boolean;
  target: Target;
  /** Every page that is this agent's: its task page and its session. The row is marked on each. */
  pages: Target[];
  /** The prefill "Ask Mainloop" puts in the composer for this row. */
  ask?: string;
  /** 1 for a task listed under the task that directs it. */
  depth: 0 | 1;
}

export interface SpaceRow {
  id: string;
  /** The project's short name (`widgets` for `acme/widgets`), or `main` for the main thread's. */
  label: string;
  /** The full name, for a tooltip, when it says more than the label. */
  title?: string;
  /** What its agents are doing, as counts: `1 needs you · 2 working`, or `idle`. */
  detail: string;
  /** The most urgent of its agents' dots. */
  tone: Tone;
  /** How many of its agents need the owner (a question or a failure), and how many are working. */
  needs: number;
  working: number;
  target: Target;
}

export interface AgentColumnModel {
  spaces: SpaceRow[];
  /** Grouped by space, in the spaces' order. */
  agents: AgentRow[];
  /** What the main thread speaks for: the Needs-you count every badge shows, and the agents working. */
  main: { needs: number; working: number };
}

/** Most urgent first. A space shows the first of these that any of its agents has. */
const URGENCY: readonly Tone[] = ['attention', 'bad', 'active', 'unseen'];

function spaceTone(agents: AgentRow[]): Tone {
  return URGENCY.find((tone) => agents.some((agent) => agent.tone === tone)) ?? 'muted';
}

/** The agents with an amber dot: what is still moving. The status bar's **N working** counts them. */
export function workingCount(agents: AgentRow[]): number {
  return agents.filter((agent) => agent.role !== 'main' && agent.tone === 'active').length;
}

/**
 * `1 needs you · 2 working · 1 problem · 1 finished`, each part left out when it is zero; `idle`
 * for none.
 */
const countTones = (agents: AgentRow[], ...tones: Tone[]) =>
  agents.filter((agent) => tones.includes(agent.tone)).length;

function spaceDetail(agents: AgentRow[]): string {
  const count = (...tones: Tone[]) => countTones(agents, ...tones);
  const needs = count('attention', 'bad');
  const working = count('active');
  const problems = agents.filter((agent) => agent.problem).length;
  const finished = count('unseen');
  const parts: string[] = [];
  if (needs > 0) parts.push(`${needs} ${needs === 1 ? 'needs you' : 'need you'}`);
  if (working > 0) parts.push(`${working} working`);
  if (problems > 0) parts.push(`${problems} ${problems === 1 ? 'problem' : 'problems'}`);
  if (finished > 0) parts.push(`${finished} finished`);
  return parts.length > 0 ? parts.join(' · ') : 'idle';
}

/** The owner action that names a task, when one does: it says what is needed better than a status. */
function actionFor(attention: AttentionItem[], taskId: string): AttentionItem | undefined {
  return needsYouItems(attention).find((item) => taskIdOf(item.task_id, item.href) === taskId);
}

/**
 * Whether a task is an agent in the list: it is the project's standing supervisor (always, at rest
 * or not), it has not ended, it needs the owner, or it finished in the last seven days and has not
 * been looked at. A finished task that has been looked at leaves the
 * list; its project's page still has it.
 */
function isListed(
  view: TaskView,
  source: ExplorerSource,
  named: ReadonlySet<string>,
  now: number
): boolean {
  if (view.task.standing || named.has(view.task.id)) return true;
  if (workGroup(view, null, now) !== null) return true;
  return (
    view.task.status === 'completed' &&
    Date.parse(view.task.updated_at) >= now - FINISHED_WINDOW_MS &&
    isUnseen(source.seen, view.task.id, view.task.updated_at)
  );
}

function taskAgent(
  view: TaskView,
  source: ExplorerSource,
  space: { id: string; label: string },
  depth: 0 | 1,
  now: number
): AgentRow {
  const { task } = view;
  const node = stagedTaskNode(view, source, 'agents', now);
  const action = actionFor(source.attention, task.id);
  const supervisor = task.mode === 'coordination';
  const detail = action?.needed ?? node.detail;
  const stuck = workGroup(view, null, now) === 'problem';
  const problem = !action && stuck;
  const page: Target = { to: 'task', id: task.id };
  const sessionId = currentAttempt(view)?.session_id;
  const conversation: Target | null = sessionId ? { to: 'session', id: sessionId } : null;
  return {
    id: `task:${task.id}`,
    space: space.id,
    spaceLabel: space.label,
    // The standing supervisor is named for what it is: its space's name is already over it.
    label: task.standing ? 'Supervisor' : node.label,
    role: supervisor ? 'supervisor' : 'worker',
    standing: task.standing === true,
    detail:
      supervisor && !task.standing ? ['Supervisor', detail].filter(Boolean).join(' · ') : detail,
    at: node.at,
    tone: action ? 'attention' : (node.tone ?? 'muted'),
    problem,
    // An agent that is still going opens on its conversation, where it can be read and told
    // things. One that has ended, has no session yet, or that Mainloop is recovering (its session
    // may be gone) opens on its task page: what happened, its pull request, and what can be done.
    target: conversation && !isTerminal(task.status) && !stuck ? conversation : page,
    pages: conversation ? [page, conversation] : [page],
    ask: node.ask,
    depth
  };
}

function sessionAgent(session: Session, space: { id: string; label: string }): AgentRow {
  const node = sessionNode(session, 'agents/');
  const target: Target = { to: 'session', id: session.id };
  return {
    id: `session:${session.id}`,
    space: space.id,
    spaceLabel: space.label,
    label: node.label,
    role: 'session',
    standing: false,
    detail: node.detail,
    tone: node.tone ?? 'muted',
    problem: false,
    target,
    pages: [target],
    ask: node.ask,
    depth: 0
  };
}

const byCreated = (a: TaskView, b: TaskView) => a.task.created_at.localeCompare(b.task.created_at);

/** The standing supervisor first, then the order the agents were started in. */
const byStanding = (a: TaskView, b: TaskView) =>
  Number(b.task.standing === true) - Number(a.task.standing === true) || byCreated(a, b);

/**
 * The two lists. Spaces are Mainloop, then the projects in the order the server lists them. Within
 * a space the project's standing supervisor comes first, and the other agents keep the order they
 * were started in, so a row does not move as its state changes; a task that another listed task directs follows that task, one step in. `page` is the
 * page being shown.
 */
export function buildAgentColumn(
  source: ExplorerSource,
  page: Target | null = null
): AgentColumnModel {
  const now = source.now ?? Date.now();
  const work = ownerWork(source.attention, source.tasks, now);
  // The task whose page is showing stays listed while it shows, so opening a finished agent does
  // not take its row away under the owner; it leaves the list when they move on.
  const open =
    page?.to === 'task'
      ? page.id
      : page?.to === 'session'
        ? taskForSession(source.tasks, page.id)?.view.task.id
        : undefined;
  const listed = source.tasks.filter(
    (view) => view.task.id === open || isListed(view, source, work.named, now)
  );
  const listedIds = new Set(listed.map((view) => view.task.id));

  // Not "Mainloop": a project can be called that, and this is the main thread's own space.
  const mainloop = { id: MAINLOOP_SPACE, label: 'main' };
  const projects = source.projects.map((project) => ({
    id: project.id,
    label: shortProject(project.full_name),
    title: project.full_name
  }));
  const known = new Set(projects.map((project) => project.id));
  // A task or session whose project is not loaded (or that has none) is the orchestrator's own.
  const spaceId = (projectId: string | null | undefined) =>
    projectId && known.has(projectId) ? projectId : MAINLOOP_SPACE;

  const agentsOf = (space: { id: string; label: string }): AgentRow[] => {
    const rows: AgentRow[] = [];
    const tasks = listed.filter((view) => spaceId(view.task.project_id) === space.id);
    const directed = (view: TaskView) =>
      view.task.parent_task_id != null && listedIds.has(view.task.parent_task_id);
    // A directed task is listed with the task that directs it, whichever project it works in.
    const add = (view: TaskView, depth: 0 | 1) => {
      rows.push(taskAgent(view, source, space, depth, now));
      for (const child of listed
        .filter((candidate) => candidate.task.parent_task_id === view.task.id)
        .sort(byCreated)) {
        add(child, 1);
      }
    };
    for (const view of tasks.filter((candidate) => !directed(candidate)).sort(byStanding)) {
      add(view, 0);
    }
    for (const session of source.sessions) {
      if (!isCurrent(session) || taskForSession(source.tasks, session.id)) continue;
      if (spaceId(session.project_id) === space.id) rows.push(sessionAgent(session, space));
    }
    return rows;
  };

  const spaces: SpaceRow[] = [];
  const agents: AgentRow[] = [];

  // The main thread speaks for everything it directs: it needs the owner while anything does (the
  // same count as every Needs-you badge), and is working while any task is.
  const others = [mainloop, ...projects].map((space) => ({ space, rows: agentsOf(space) }));
  const all = others.flatMap(({ rows }) => rows);
  const counts = { needs: needsTotal(work), working: countTones(all, 'active') };
  const state = mainloopState(counts.needs, counts.working);
  const main: AgentRow = {
    id: 'main',
    space: MAINLOOP_SPACE,
    spaceLabel: mainloop.label,
    label: 'Mainloop',
    role: 'main',
    standing: false,
    detail: state.label,
    tone: state.tone,
    problem: false,
    target: { to: 'chat' },
    pages: [{ to: 'chat' }],
    depth: 0
  };

  for (const { space, rows } of others) {
    const own = space.id === MAINLOOP_SPACE ? [main, ...rows] : rows;
    spaces.push({
      id: space.id,
      label: space.label,
      title: 'title' in space && space.title !== space.label ? String(space.title) : undefined,
      // The main thread is not counted with the agents beside it: it speaks for all of them. With
      // none beside it, its own state is the space's.
      detail:
        space.id === MAINLOOP_SPACE && rows.length === 0
          ? state.label.toLowerCase()
          : spaceDetail(rows),
      tone: spaceTone(own),
      needs: countTones(rows, 'attention', 'bad'),
      working: countTones(rows, 'active'),
      target: space.id === MAINLOOP_SPACE ? { to: 'chat' } : { to: 'project', id: space.id }
    });
    agents.push(...own);
  }
  return { spaces, agents, main: counts };
}

/** The agent whose page is showing, if it is in the list. */
export function currentAgent(agents: AgentRow[], page: Target | null): AgentRow | undefined {
  if (!page) return undefined;
  return agents.find((agent) => agent.pages.some((own) => sameTarget(own, page)));
}

/**
 * The space the page being shown belongs to: the project of its task, session or project page, and
 * Mainloop for the main chat and every page that belongs to no project (the Inbox, Settings,
 * orchestration health, schedules).
 */
export function spaceOfPage(page: Target | null, source: ExplorerSource): string {
  const known = (projectId: string | null | undefined) =>
    projectId && source.projects.some((project) => project.id === projectId)
      ? projectId
      : MAINLOOP_SPACE;
  if (!page) return MAINLOOP_SPACE;
  if (page.to === 'project') return known(page.id);
  if (page.to === 'task') {
    return known(source.tasks.find((view) => view.task.id === page.id)?.task.project_id);
  }
  if (page.to === 'session') {
    const owner = taskForSession(source.tasks, page.id);
    if (owner) return known(owner.view.task.project_id);
    return known(source.sessions.find((session) => session.id === page.id)?.project_id);
  }
  return MAINLOOP_SPACE;
}

export interface TabRow {
  /** Unique in the strip: `home`, an agent's id, or `page`. */
  id: string;
  label: string;
  /** The full name, for a tooltip, when the label may be cut short. */
  title?: string;
  /** The agent's dot. A tab that is a page, not an agent, has none. */
  tone?: Tone;
  /** Where the tab leads. The tab for the page being shown, when it is no agent's, leads nowhere. */
  target?: Target;
  current: boolean;
}

/** What a page that is no agent's is called on its tab, by the first part of its path. */
const PAGE_TABS: Record<string, string> = {
  inbox: 'Inbox',
  settings: 'Settings',
  health: 'Health',
  schedules: 'Schedules',
  transcripts: 'Transcripts',
  agents: 'New agent',
  workspaces: 'Workspace',
  tasks: 'Task',
  sessions: 'Session',
  projects: 'Overview'
};

/**
 * The tab strip over the page: the current space's agents, as a terminal workspace manager shows
 * one tab per agent in the space. In the main thread's space, `main` (the main chat) and then its
 * agents. In a project with a standing supervisor, `supervisor` and then its workers, in the
 * column's order. In a project that has none yet, `overview` (the project page) and then its agents.
 * When the page being shown is none of those (the Inbox, Settings, a supervised project's own page,
 * a superseded session), it gets a last tab of its own, so one tab is always lit.
 */
export function spaceTabs(
  model: AgentColumnModel,
  space: string,
  page: Target | null,
  pathname: string
): TabRow[] {
  const here = currentAgent(model.agents, page);
  const home: TabRow =
    space === MAINLOOP_SPACE
      ? {
          id: 'home',
          label: 'Mainloop',
          tone: model.agents.find((agent) => agent.role === 'main')?.tone,
          target: { to: 'chat' },
          current: page?.to === 'chat'
        }
      : {
          id: 'home',
          label: 'Overview',
          target: { to: 'project', id: space },
          current: page?.to === 'project' && page.id === space
        };
  const agents = model.agents.filter((agent) => agent.space === space && agent.role !== 'main');
  // A project with a standing supervisor opens on it: the supervisor is the space's first tab, and
  // the project's page is reached from the space's row.
  const supervised = agents.some((agent) => agent.standing);
  const tabs: TabRow[] = [
    ...(supervised ? [] : [home]),
    ...agents.map((agent) => ({
      id: agent.id,
      label: agent.label,
      title: agent.standing ? undefined : agent.label,
      tone: agent.tone,
      target: agent.target,
      current: agent === here
    }))
  ];
  if (!tabs.some((tab) => tab.current)) {
    const part = pathname.split('/').filter(Boolean)[0] ?? '';
    tabs.push({ id: 'page', label: PAGE_TABS[part] ?? (part || 'page'), current: true });
  }
  return tabs;
}

/**
 * The tabs while the agents or their spaces are still being read: only what is certain. A page that
 * is no agent's (the main chat, the Inbox, Settings) is the main thread's, so `main` and that page's
 * own tab can be drawn at once. On a task's, a session's or a project's page the space is not known
 * yet, so there are no tabs until it is: a tab is never drawn in one space and then moved.
 */
export function loadingTabs(
  model: AgentColumnModel,
  page: Target | null,
  pathname: string
): TabRow[] {
  if (page?.to === 'task' || page?.to === 'session' || page?.to === 'project') return [];
  const main = model.agents.filter((agent) => agent.role === 'main');
  return spaceTabs({ ...model, agents: main }, MAINLOOP_SPACE, page, pathname);
}

/**
 * What "new" is called in a space, for the sidebar's **new** and the strip's "+": a task in the
 * project, or a word with Mainloop in the main thread's space.
 */
export function newLabel(model: AgentColumnModel, space: string): string {
  const row = model.spaces.find((candidate) => candidate.id === space);
  return space === MAINLOOP_SPACE || !row ? 'New: ask Mainloop' : `New task in ${row.label}`;
}

/** What "new" puts in the main chat's composer, unsent: work starts from the chat. */
export function newTaskPrefill(model: AgentColumnModel, space: string): string | undefined {
  if (space === MAINLOOP_SPACE) return undefined;
  const row = model.spaces.find((candidate) => candidate.id === space);
  return row ? `Start a task in ${row.title ?? row.label}: ` : undefined;
}
