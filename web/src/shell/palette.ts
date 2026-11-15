/**
 * The command palette's rules: what it lists and how a typed query narrows and orders the list.
 * Pure, so they test without a DOM. Everything it can jump to comes from the explorer's tree, so the
 * palette and the sidebar never disagree about what exists.
 */
import type { ExplorerNode, Target } from '@/features/explorer';
import { THEMES, type ThemeId } from './themes';

/** What running an item does. The palette turns it into a navigation, a theme change or a prefill. */
export type PaletteAction =
  | { do: 'open'; target: Target }
  | { do: 'settings' }
  | { do: 'schedules' }
  | { do: 'theme'; theme: ThemeId }
  | { do: 'ask'; text: string }
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

const KINDS: Partial<Record<Target['to'], string>> = {
  task: 'Task',
  session: 'Session',
  workspace: 'Workspace',
  project: 'Project',
  schedule: 'Schedule',
  needs: 'Needs you',
  problems: 'Problem'
};

/** One key per place, so a task listed under Needs you and under its project appears once. */
function targetKey(target: Target): string {
  const id = 'id' in target ? target.id : 'item' in target ? (target.item ?? '') : '';
  return `${target.to}:${id}`;
}

/**
 * Every row of the tree that leads somewhere, once each, in the tree's own order (what needs the
 * owner first). Group headings and the pages listed above are left out.
 */
function treeItems(nodes: ExplorerNode[]): PaletteItem[] {
  const seen = new Set<string>();
  const items: PaletteItem[] = [];
  const visit = (node: ExplorerNode) => {
    const { target } = node;
    const kind = target ? KINDS[target.to] : undefined;
    // A heading's own target (the whole Needs you list) is a page, already listed.
    const isRow =
      target && kind && (('id' in target && target.id) || ('item' in target && target.item));
    if (target && kind && isRow && !node.note) {
      const key = targetKey(target);
      if (!seen.has(key)) {
        seen.add(key);
        items.push({
          id: key,
          label: node.label,
          detail: [node.project, node.detail].filter(Boolean).join(' · ') || undefined,
          kind,
          action: { do: 'open', target },
          ask: node.ask
        });
      }
    }
    node.children.forEach(visit);
  };
  nodes.forEach(visit);
  return items;
}

/** Everything the palette lists before a query narrows it: pages, the tree's rows, then themes. */
export function paletteItems(nodes: ExplorerNode[]): PaletteItem[] {
  const themes: PaletteItem[] = THEMES.map(({ id, label }) => ({
    id: `theme:${id}`,
    label: `Theme: ${label}`,
    kind: 'Theme',
    action: { do: 'theme', theme: id }
  }));
  return [...PAGES, ...treeItems(nodes), ...themes];
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
