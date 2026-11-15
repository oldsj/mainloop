import { describe, expect, it } from 'vitest';
import type { ExplorerNode } from '@/features/explorer';
import { filterItems, PALETTE_LIMIT, paletteItems } from './palette';
import { THEMES } from './themes';

const task = (id: string, label: string, extra: Partial<ExplorerNode> = {}): ExplorerNode => ({
  id: `x/task:${id}`,
  label,
  target: { to: 'task', id },
  ask: `About task ${label} (${id}): `,
  children: [],
  ...extra
});

const nodes: ExplorerNode[] = [
  { id: 'mainloop', label: 'Mainloop', target: { to: 'chat' }, children: [] },
  {
    id: 'needs',
    label: 'Needs you',
    count: 1,
    target: { to: 'needs' },
    children: [
      {
        id: 'needs/att:1',
        label: 'Pick a region',
        detail: 'Choose eu or us',
        target: { to: 'needs', item: 'q-1' },
        children: []
      },
      task('t1', 'Fix login', { project: 'widgets', detail: 'Failed' })
    ]
  },
  {
    id: 'working',
    label: 'Working',
    count: 0,
    children: [{ id: 'working/none', label: 'Nothing running', note: true, children: [] }]
  },
  {
    id: 'projects',
    label: 'Projects',
    count: 1,
    children: [
      {
        id: 'projects/p1',
        label: 'acme/widgets',
        target: { to: 'project', id: 'p1' },
        children: [task('t1', 'Fix login'), task('t2', 'Queue backend', { detail: 'Running' })]
      }
    ]
  }
];

describe('what the palette lists', () => {
  const items = paletteItems(nodes);

  it('starts with the pages, then the tree’s rows, then the themes', () => {
    expect(items.map((item) => item.kind)).toEqual([
      ...Array.from({ length: 8 }, () => 'Page'),
      'Needs you',
      'Task',
      'Project',
      'Task',
      ...THEMES.map(() => 'Theme')
    ]);
    expect(items[0]?.label).toBe('Main chat');
  });

  it('lists a task once, though the tree shows it under Needs you and under its project', () => {
    const fixes = items.filter((item) => item.label === 'Fix login');
    expect(fixes).toHaveLength(1);
    // The first place the tree shows it gives its words: the project and what state it is in.
    expect(fixes[0]).toMatchObject({ detail: 'widgets · Failed', kind: 'Task' });
  });

  it('leaves out group headings and placeholder rows', () => {
    const labels = items.map((item) => item.label);
    expect(labels).not.toContain('Working');
    expect(labels).not.toContain('Nothing running');
    expect(labels).not.toContain('Mainloop');
  });

  it('carries each row’s Ask Mainloop prefill', () => {
    expect(items.find((item) => item.label === 'Queue backend')?.ask).toBe(
      'About task Queue backend (t2): '
    );
  });
});

describe('how a query narrows the list', () => {
  const items = paletteItems(nodes);
  const labels = (query: string) => filterItems(items, query).map((item) => item.label);

  it('keeps everything, in order, with no query', () => {
    expect(filterItems(items, '   ')).toEqual(items);
  });

  it('needs every word, in the label or in the words beside it', () => {
    expect(labels('fix login')).toEqual(['Fix login', 'Ask Mainloop: fix login']);
    // "widgets" is the task's project, not in its title.
    expect(labels('fix widgets')).toEqual(['Fix login', 'Ask Mainloop: fix widgets']);
    expect(labels('fix nothing')).toEqual(['Ask Mainloop: fix nothing']);
  });

  it('puts a label that starts with the query first, then a word that starts with it, then the rest', () => {
    // Both Inbox pages start with it; "Main chat" and "Fix login" only contain it.
    const found = labels('in');
    expect(found.slice(0, 2)).toEqual(['Inbox: Needs you', 'Inbox: Problems']);
    expect(found.indexOf('Main chat')).toBeGreaterThan(1);
    expect(found.indexOf('Fix login')).toBeGreaterThan(1);
    // A word in the label that starts with it comes before a match on the kind alone.
    expect(labels('pro')).toEqual([
      'Inbox: Problems',
      'Import a project',
      'acme/widgets',
      'Ask Mainloop: pro'
    ]);
  });

  it('matches a kind, so "theme" lists the themes', () => {
    expect(labels('theme').slice(0, THEMES.length)).toEqual(
      THEMES.map(({ label }) => `Theme: ${label}`)
    );
  });

  it('always ends a query with a row that asks Mainloop the query itself', () => {
    const last = filterItems(items, 'what is running').at(-1);
    expect(last).toMatchObject({ kind: 'Ask', action: { do: 'ask', text: 'what is running' } });
  });

  it('draws no more than the limit', () => {
    const many = Array.from({ length: 200 }, (_, index) => task(`t${index}`, `Task ${index}`));
    expect(filterItems(paletteItems(many), '')).toHaveLength(PALETTE_LIMIT);
    expect(filterItems(paletteItems(many), 'task')).toHaveLength(PALETTE_LIMIT);
  });
});

describe('transcript search', () => {
  it('is a page the palette jumps to', () => {
    expect(filterItems(paletteItems([]), 'transcripts')[0]).toMatchObject({
      label: 'Search transcripts',
      action: { do: 'transcripts' }
    });
  });
});
