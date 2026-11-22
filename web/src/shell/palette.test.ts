import { describe, expect, it } from 'vitest';
import type { ExplorerSource } from '@/features/explorer';
import { attentionItem, problemAttentionItem, queueItem } from '@/features/inbox/test/fixtures';
import { keepValid, projectRowSchema } from '@/features/projects/model';
import { project } from '@/features/projects/test/fixtures';
import { schedule } from '@/features/schedules/test/fixtures';
import { session } from '@/features/sessions/test/fixtures';
import { workspaceFixture } from '@/test/fixtures';
import { NOW, view } from '@/test/fixtures/tasks';
import { commandItems, filterItems, PALETTE_LIMIT, paletteItems } from './palette';
import { THEMES } from './themes';

const EMPTY: ExplorerSource = {
  tasks: [],
  attention: [],
  sessions: [],
  workspaces: [],
  projects: [],
  schedules: [],
  now: NOW
};

const source = (overrides: Partial<ExplorerSource> = {}): ExplorerSource => ({
  ...EMPTY,
  ...overrides
});

const at = (minutesAgo: number) => new Date(NOW - minutesAgo * 60_000).toISOString();
const PAGES = Array.from({ length: 8 }, () => 'Page');
const kinds = (items: ReturnType<typeof paletteItems>) => items.map((item) => item.kind);

/** A question with no task, a failed task in a project that can be retried, and a running task. */
const work = source({
  attention: [
    attentionItem(
      queueItem({ id: 'q-1', title: 'Pick a region', content: 'Choose eu or us', task_id: null })
    )
  ],
  tasks: [
    view({ id: 't2', title: 'Queue backend', status: 'running', project_id: null }),
    view({ id: 't1', title: 'Fix login', status: 'failed', project_id: 'p1' })
  ],
  projects: keepValid([project({ id: 'p1', full_name: 'acme/widgets' })], projectRowSchema)
});

describe('what the palette lists', () => {
  const items = paletteItems(work);

  it('starts with the pages, then the places, then the themes', () => {
    expect(kinds(items)).toEqual([
      ...PAGES,
      'Needs you',
      'Task',
      'Task',
      'Project',
      ...THEMES.map(() => 'Theme')
    ]);
    expect(items[0]?.label).toBe('Main chat');
  });

  it('lists what needs the owner first, then problems, tasks, loose sessions, workspaces, projects and schedules', () => {
    // Given one of everything the palette can jump to.
    const everything = source({
      ...work,
      attention: [
        ...work.attention,
        problemAttentionItem({ id: 'reconciliation:t9', task_id: 't9', title: 'Stalled build' })
      ],
      sessions: [session({ id: 'loose', title: 'Scratch', status: 'active' })],
      workspaces: [workspaceFixture({ workspace_id: 'workspace-abcdef' })],
      schedules: [schedule({ id: 'sch-1' })]
    });

    // When it is listed.
    const places = paletteItems(everything).slice(PAGES.length, -THEMES.length);

    // Then each kind follows the one more urgent than it, and opens its own page.
    expect(places.map((item) => [item.kind, item.label, item.action])).toEqual([
      ['Needs you', 'Pick a region', { do: 'open', target: { to: 'needs', item: 'q-1' } }],
      [
        'Problem',
        'Stalled build',
        { do: 'open', target: { to: 'problems', item: 'reconciliation:t9' } }
      ],
      ['Task', 'Fix login', { do: 'open', target: { to: 'task', id: 't1' } }],
      ['Task', 'Queue backend', { do: 'open', target: { to: 'task', id: 't2' } }],
      ['Session', 'Scratch', { do: 'open', target: { to: 'session', id: 'loose' } }],
      [
        'Workspace',
        'Workspace workspac',
        { do: 'open', target: { to: 'workspace', id: 'workspace-abcdef' } }
      ],
      ['Project', 'acme/widgets', { do: 'open', target: { to: 'project', id: 'p1' } }],
      ['Schedule', 'Nightly triage', { do: 'open', target: { to: 'schedule', id: 'sch-1' } }]
    ]);
  });

  it('lists a task once, though it both needs the owner and is one of the tasks', () => {
    const fixes = items.filter((item) => item.label === 'Fix login');
    expect(fixes).toHaveLength(1);
    // Its words are the project and what state it is in.
    expect(fixes[0]).toMatchObject({ detail: 'widgets · Failed', kind: 'Task' });
  });

  it('puts the tasks that need the owner first, then the rest by latest change', () => {
    const tasks = [
      view({ id: 'old', title: 'Old', status: 'running', updated_at: at(30) }),
      view({ id: 'broke', title: 'Broke', status: 'failed', updated_at: at(20) }),
      view({ id: 'new', title: 'New', status: 'running', updated_at: at(10) })
    ];
    const listed = paletteItems(source({ tasks })).filter((item) => item.kind === 'Task');
    expect(listed.map((item) => item.label)).toEqual(['Broke', 'New', 'Old']);
  });

  it('leaves a task’s own session and a finished session to their pages', () => {
    // Given a task whose attempt runs in session-1, and a session that has ended.
    const sessions = [
      session({ id: 'session-1', title: 'Task session', status: 'active' }),
      session({ id: 'done', title: 'Old', status: 'completed' })
    ];
    const listed = paletteItems(source({ tasks: [view({ id: 't1' })], sessions }));
    expect(kinds(listed)).not.toContain('Session');
  });

  it('says a paused schedule is paused, and gives a live one its timing', () => {
    const schedules = [
      schedule({ id: 'sch-1', config: { ...schedule().config, paused: true } }),
      schedule({ id: 'sch-2' })
    ];
    const listed = paletteItems(source({ schedules })).filter((item) => item.kind === 'Schedule');
    expect(listed.map((item) => item.detail)).toEqual(['Paused', schedule().config.schedule]);
    expect(listed[0]?.ask).toBe('About schedule Nightly triage: ');
  });

  it('lists only the pages and the themes when there is nothing to jump to', () => {
    expect(kinds(paletteItems(EMPTY))).toEqual([...PAGES, ...THEMES.map(() => 'Theme')]);
  });

  it('carries each row’s Ask Mainloop prefill', () => {
    const ask = (label: string) => items.find((item) => item.label === label)?.ask;
    expect(ask('Queue backend')).toBe('About task Queue backend (t2): ');
    expect(ask('acme/widgets')).toBe('In project acme/widgets: ');
    expect(ask('Pick a region')).toBe('About question Pick a region: ');
  });

  it('names an owner action’s task in its prefill, and asks nothing about a problem', () => {
    const attention = [
      attentionItem(
        queueItem({
          id: 'q-2',
          item_type: 'approval',
          title: 'Approve a push',
          task_id: 'task-1234abcd99'
        })
      ),
      problemAttentionItem()
    ];
    const listed = paletteItems(source({ attention })).slice(PAGES.length, -THEMES.length);
    expect(listed.map((item) => [item.kind, item.ask])).toEqual([
      ['Needs you', 'About approval Approve a push (task task-123): '],
      ['Problem', undefined]
    ]);
  });
});

describe('how a query narrows the list', () => {
  const items = paletteItems(work);
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
    const tasks = Array.from({ length: 200 }, (_, index) =>
      view({ id: `t${index}`, title: `Task ${index}` })
    );
    const many = paletteItems(source({ tasks }));
    expect(filterItems(many, '')).toHaveLength(PALETTE_LIMIT);
    expect(filterItems(many, 'task')).toHaveLength(PALETTE_LIMIT);
  });
});

describe('the shell’s commands', () => {
  const desktop = { newLabel: 'New task in widgets', keys: true };

  it('lists the prefix keys’ actions as rows, each with its key, on the desktop', () => {
    expect(commandItems(desktop).map((item) => [item.label, item.detail, item.kind])).toEqual([
      ['Next tab', 'Ctrl+B n', 'Command'],
      ['Previous tab', 'Ctrl+B p', 'Command'],
      ['New task in widgets', 'Ctrl+B c', 'Command'],
      ['Hide or show the sidebar', 'Ctrl+B b', 'Command'],
      ['Go to the spaces', 'Ctrl+B w', 'Command']
    ]);
    expect(commandItems(desktop)[0]?.action).toEqual({ do: 'command', command: 'next-tab' });
  });

  it('names no keys on the phone, where there are none, and calls the sidebar what it is there', () => {
    const items = commandItems({ newLabel: 'New: ask Mainloop', keys: false });

    expect(items.every((item) => item.detail === undefined)).toBe(true);
    expect(items.map((item) => item.label)).toContain('Show or hide the spaces and agents');
  });

  it('sits after the pages and before everything there is to jump to', () => {
    const kindsInOrder = paletteItems(EMPTY, desktop).map((item) => item.kind);
    const first = kindsInOrder.indexOf('Command');

    expect(kindsInOrder.slice(0, first).every((kind) => kind === 'Page')).toBe(true);
    expect(kindsInOrder.slice(first, first + 5)).toEqual(Array(5).fill('Command'));
    expect(kindsInOrder.slice(first + 5).every((kind) => kind === 'Theme')).toBe(true);
  });

  it('is found by what it does or by its key', () => {
    const items = paletteItems(EMPTY, desktop);

    expect(filterItems(items, 'next tab')[0]?.label).toBe('Next tab');
    expect(filterItems(items, 'sidebar')[0]?.label).toBe('Hide or show the sidebar');
    expect(filterItems(items, 'spaces')[0]?.label).toBe('Go to the spaces');
    // The prefix finds all five, and only them.
    expect(
      filterItems(items, 'ctrl+b')
        .filter((item) => item.kind !== 'Ask')
        .map((item) => item.kind)
    ).toEqual(Array(5).fill('Command'));
  });

  it('is left out when the caller gives none', () => {
    expect(paletteItems(EMPTY).some((item) => item.kind === 'Command')).toBe(false);
  });
});

describe('transcript search', () => {
  it('is a page the palette jumps to', () => {
    expect(filterItems(paletteItems(EMPTY), 'transcripts')[0]).toMatchObject({
      label: 'Search transcripts',
      action: { do: 'transcripts' }
    });
  });
});
