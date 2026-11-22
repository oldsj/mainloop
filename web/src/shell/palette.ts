/**
 * The command palette's rules: what it lists and how a typed query narrows and orders the list.
 * Pure, so they test without a DOM. Everything it can jump to comes from the reads the sidebar is
 * built from, and each row's words from the same rules, so the two never disagree about what exists.
 */
import {
  askPrefill,
  sessionNode,
  shortId,
  stagedTaskNode,
  workspaceNode,
  type ExplorerSource,
  type Target
} from '@/features/explorer';
import { attentionKey, type AttentionItem } from '@/features/inbox/model';
import { ownerWork } from '@/features/inbox/ownerWork';
import { isCurrent } from '@/lib/sessionStatus';
import { taskForSession } from '@/lib/taskState';
import { COMMAND_KEYS, type ShellCommand } from './commands';
import { THEMES, type ThemeId } from './themes';

/** What running an item does. The palette turns it into a navigation, a theme change or a prefill. */
export type PaletteAction =
  | { do: 'open'; target: Target }
  | { do: 'settings' }
  | { do: 'schedules' }
  | { do: 'theme'; theme: ThemeId }
  | { do: 'ask'; text: string }
  | { do: 'command'; command: ShellCommand }
  | { do: 'transcripts' };

export interface PaletteItem {
  /** Unique in the list. */
  id: string;
  label: string;
  /** Muted words after the label: the project, the state. */
  detail?: string;
  /** What kind of thing it is, shown on the right: Page, Task, Project, Theme. */
  kind: string;
  action: PaletteAction;
  /** The "Ask Mainloop" prefill for this thing, run with the modifier and Enter. */
  ask?: string;
}

const PAGES: PaletteItem[] = [
  {
    id: 'page:chat',
    label: 'Main chat',
    kind: 'Page',
    action: { do: 'open', target: { to: 'chat' } }
  },
  {
    id: 'page:needs',
    label: 'Inbox: Needs you',
    kind: 'Page',
    action: { do: 'open', target: { to: 'needs' } }
  },
  {
    id: 'page:problems',
    label: 'Inbox: Problems',
    kind: 'Page',
    action: { do: 'open', target: { to: 'problems' } }
  },
  {
    id: 'page:health',
    label: 'Orchestration health',
    kind: 'Page',
    action: { do: 'open', target: { to: 'health' } }
  },
  { id: 'page:schedules', label: 'Scheduled runs', kind: 'Page', action: { do: 'schedules' } },
  {
    id: 'page:transcripts',
    label: 'Search transcripts',
    kind: 'Page',
    action: { do: 'transcripts' }
  },
  { id: 'page:settings', label: 'Settings', kind: 'Page', action: { do: 'settings' } },
  {
    id: 'page:import',
    label: 'Import a project',
    kind: 'Page',
    action: { do: 'open', target: { to: 'import' } }
  }
];

/** What an owner action is called in an Ask Mainloop prefill. */
const ACTION_NAMES: Record<string, string> = {
  approval: 'approval',
  question: 'question',
  owner_decision: 'decision'
};

/** "About question Which branch?: ", naming the action's task when it has one. */
function actionAsk(item: AttentionItem): string {
  const name = ACTION_NAMES[item.kind] ?? 'request';
  return item.task_id
    ? `About ${name} ${item.title} (task ${shortId(item.task_id)}): `
    : `About ${name} ${item.title}: `;
}

/** One key per place, so a task that needs the owner is not listed again among the other tasks. */
function targetKey(target: Target): string {
  const id = 'id' in target ? target.id : 'item' in target ? (target.item ?? '') : '';
  return `${target.to}:${id}`;
}

/** A row before it has its id, which is its target's key. */
type Place = Omit<PaletteItem, 'id' | 'action'> & { target: Target };

/**
 * Everything that has a page of its own, once each, what needs the owner first: the owner actions,
 * the problems Mainloop is handling, every task (those that need the owner, then the rest by latest
 * change), the live sessions that belong to no task, the workspaces, the projects and the schedules.
 */
function placeItems(source: ExplorerSource): PaletteItem[] {
  const now = source.now ?? Date.now();
  const work = ownerWork(source.attention, source.tasks, now);
  const newestFirst = [...source.tasks].sort(
    (a, b) => Date.parse(b.task.updated_at) - Date.parse(a.task.updated_at)
  );

  const places: Place[] = [
    // An owner action opens the answer view on its own row, whether or not it belongs to a task.
    ...work.needs.map((item) => ({
      label: item.title,
      detail: item.needed,
      kind: 'Needs you',
      target: { to: 'needs', item: attentionKey(item) } satisfies Target,
      ask: actionAsk(item)
    })),
    // A problem is Mainloop's own recovery work, with no decision to ask about.
    ...work.problems.map((item) => ({
      label: item.title,
      detail: item.needed,
      kind: 'Problem',
      target: { to: 'problems', item: attentionKey(item) } satisfies Target
    })),
    ...[...work.needsTasks, ...newestFirst].map((view) => {
      const node = stagedTaskNode(view, source, 'palette', now);
      return {
        label: node.label,
        detail: [node.project, node.detail].filter(Boolean).join(' · ') || undefined,
        kind: 'Task',
        target: { to: 'task', id: view.task.id } satisfies Target,
        ask: node.ask
      };
    }),
    // A task's session is reached from its task; only a session with no task is listed on its own.
    ...source.sessions
      .filter((session) => isCurrent(session) && !taskForSession(source.tasks, session.id))
      .map((session) => {
        const node = sessionNode(session, 'palette/');
        return {
          label: node.label,
          detail: node.detail,
          kind: 'Session',
          target: { to: 'session', id: session.id } satisfies Target,
          ask: node.ask
        };
      }),
    ...source.workspaces.map((workspace) => {
      const node = workspaceNode(workspace, 'palette/');
      return {
        label: node.label,
        detail: node.detail,
        kind: 'Workspace',
        target: { to: 'workspace', id: workspace.workspace_id } satisfies Target
      };
    }),
    ...source.projects.map((project) => {
      const target: Target = { to: 'project', id: project.id };
      return {
        label: project.full_name,
        kind: 'Project',
        target,
        ask: askPrefill({ label: project.full_name }, target)
      };
    }),
    ...source.schedules.map((schedule) => {
      const target: Target = { to: 'schedule', id: schedule.id };
      return {
        label: schedule.config.name,
        detail: schedule.config.paused ? 'Paused' : schedule.config.schedule,
        kind: 'Schedule',
        target,
        ask: askPrefill({ label: schedule.config.name }, target)
      };
    })
  ];

  const seen = new Set<string>();
  const items: PaletteItem[] = [];
  for (const { target, ...place } of places) {
    const key = targetKey(target);
    if (seen.has(key)) continue;
    seen.add(key);
    items.push({ id: key, ...place, action: { do: 'open', target } });
  }
  return items;
}

export interface CommandOptions {
  /** What "new" is called in the space the owner is in: "New task in widgets". */
  newLabel: string;
  /** Whether the prefix keys are on (the desktop shell), so each command can say its key. */
  keys: boolean;
}

/**
 * The shell's own actions as rows: the same ones the prefix keys run, so they can be reached in any
 * browser, with an extension that takes the prefix, or without knowing the keys. On the desktop each
 * says its key.
 */
export function commandItems({ newLabel, keys }: CommandOptions): PaletteItem[] {
  const rows: { command: ShellCommand; label: string }[] = [
    { command: 'next-tab', label: 'Next tab' },
    { command: 'previous-tab', label: 'Previous tab' },
    { command: 'new', label: newLabel },
    {
      command: 'sidebar',
      label: keys ? 'Hide or show the sidebar' : 'Show or hide the spaces and agents'
    },
    { command: 'spaces', label: 'Go to the spaces' }
  ];
  return rows.map(({ command, label }) => ({
    id: `command:${command}`,
    label,
    detail: keys ? `Ctrl+B ${COMMAND_KEYS[command]}` : undefined,
    kind: 'Command',
    action: { do: 'command', command }
  }));
}

/**
 * Everything the palette lists before a query narrows it: pages, the shell's commands when the
 * caller gives them, the places above, then themes.
 */
export function paletteItems(source: ExplorerSource, commands?: CommandOptions): PaletteItem[] {
  const themes: PaletteItem[] = THEMES.map(({ id, label }) => ({
    id: `theme:${id}`,
    label: `Theme: ${label}`,
    kind: 'Theme',
    action: { do: 'theme', theme: id }
  }));
  return [...PAGES, ...(commands ? commandItems(commands) : []), ...placeItems(source), ...themes];
}

/**
 * How well `item` matches the query's words: 0 is no match. Every word must appear in the label or
 * the detail. A label that starts with the query ranks first, then a word that starts with it, then
 * any other place in the label, then the detail alone.
 */
function score(item: PaletteItem, words: string[]): number {
  const label = item.label.toLowerCase();
  const detail = `${item.detail ?? ''} ${item.kind}`.toLowerCase();
  let total = 0;
  for (const word of words) {
    const at = label.indexOf(word);
    if (at === 0) total += 4;
    else if (at > 0 && /[^a-z0-9]/.test(label[at - 1] ?? '')) total += 3;
    else if (at > 0) total += 2;
    else if (detail.includes(word)) total += 1;
    else return 0;
  }
  return total;
}

/** The most rows the palette draws. A query narrows the rest into view. */
export const PALETTE_LIMIT = 50;

/**
 * The items a query leaves, best match first; ties keep the list's own order. An empty query keeps
 * everything in order. A query always ends with one more row that asks Mainloop the query itself, so
 * the palette never dead-ends.
 */
export function filterItems(items: PaletteItem[], query: string): PaletteItem[] {
  const text = query.trim();
  if (!text) return items.slice(0, PALETTE_LIMIT);
  const words = text.toLowerCase().split(/\s+/);
  const matches = items
    .map((item, index) => ({ item, index, rank: score(item, words) }))
    .filter((match) => match.rank > 0)
    .sort((a, b) => b.rank - a.rank || a.index - b.index)
    .slice(0, PALETTE_LIMIT - 1)
    .map((match) => match.item);
  return [
    ...matches,
    { id: 'ask:query', label: `Ask Mainloop: ${text}`, kind: 'Ask', action: { do: 'ask', text } }
  ];
}
