import { describe, expect, it } from 'vitest';
import { hitlAttentionItem } from '@/features/inbox/test/fixtures';
import { keepValid, projectRowSchema } from '@/features/projects/model';
import { project } from '@/features/projects/test/fixtures';
import { schedule } from '@/features/schedules/test/fixtures';
import { view } from '@/test/fixtures/tasks';
import {
  askPrefill,
  buildExplorer,
  FINISHED_LIMIT,
  mainloopState,
  needsCount,
  visibleRows,
  type ExplorerSource
} from './model';
import { DEFAULT_PREFS, isExpanded } from './prefs';

const EMPTY: ExplorerSource = {
  tasks: [],
  attention: [],
  sessions: [],
  workspaces: [],
  projects: [],
  schedules: [],
  online: true
};

function source(overrides: Partial<ExplorerSource> = {}): ExplorerSource {
  return { ...EMPTY, ...overrides };
}

function topLevel(nodes: ReturnType<typeof buildExplorer>) {
  return nodes.map((node) => node.id);
}

describe('explorer order and defaults', () => {
  it('lists the top-level nodes in the owner order', () => {
    expect(topLevel(buildExplorer(EMPTY))).toEqual([
      'mainloop',
      'needs',
      'working',
      'projects',
      'schedules',
      'finished',
      'health'
    ]);
  });

  it('expands Mainloop and Needs you by default, and folds the rest', () => {
    expect(isExpanded(DEFAULT_PREFS, 'mainloop')).toBe(true);
    expect(isExpanded(DEFAULT_PREFS, 'needs')).toBe(true);
    expect(isExpanded(DEFAULT_PREFS, 'working')).toBe(false);
    expect(isExpanded(DEFAULT_PREFS, 'finished')).toBe(false);
  });

  it('shows a plain note in each empty group, and never as a target', () => {
    const nodes = buildExplorer(EMPTY);
    const needs = nodes.find((node) => node.id === 'needs');
    expect(needs?.children).toEqual([
      expect.objectContaining({ note: true, label: 'Nothing needs you' })
    ]);
    expect(needs?.children[0]?.target).toBeUndefined();
  });
});

describe('needs you', () => {
  it('counts an owner action and an owner-decision task once each', () => {
    const attention = [hitlAttentionItem('req-1')];
    const tasks = [view({ id: 'task-decide', reason: 'owner_decision' })];
    const nodes = buildExplorer(source({ attention, tasks }));
    expect(needsCount(nodes)).toBe(2);
    expect(nodes.find((node) => node.id === 'needs')?.count).toBe(2);
  });

  it('does not repeat a task that an owner action already names', () => {
    const tasks = [view({ id: 'task-x', reason: 'owner_decision' })];
    const attention = [{ ...hitlAttentionItem('req-2'), task_id: 'task-x' }];
    expect(needsCount(buildExplorer(source({ attention, tasks })))).toBe(1);
  });

  it('reports the Explorer tab count as zero with nothing waiting', () => {
    expect(needsCount(buildExplorer(EMPTY))).toBe(0);
  });
});

describe('mainloop state', () => {
  it.each([
    [1, 0, 'Waiting on you', 'attention'],
    [0, 2, 'Working', 'active'],
    [0, 0, 'Idle', 'muted']
  ])('needs %i, working %i reads as %s', (needs, working, label, tone) => {
    expect(mainloopState(needs, working)).toEqual({ label, tone });
  });
});

describe('ask prefill', () => {
  it('names a task with its short id', () => {
    expect(askPrefill({ label: 'Add retry' }, { to: 'task', id: 'task-abcdef123' })).toBe(
      'About task Add retry (task-abc): '
    );
  });

  it('names a project by its full name', () => {
    expect(askPrefill({ label: 'acme/widgets' }, { to: 'project', id: 'p1' })).toBe(
      'In project acme/widgets: '
    );
  });

  it('gives no prefill for a row that is not about a thing', () => {
    expect(askPrefill({ label: 'Health' }, { to: 'health' })).toBeUndefined();
  });

  it('puts the ask on each task row in the tree', () => {
    const nodes = buildExplorer(
      source({
        projects: keepValid([project()], projectRowSchema),
        tasks: [view({ id: 'task-zz', project_id: 'project-a', title: 'Add retry' })]
      })
    );
    const row = nodes.find((node) => node.id === 'projects')?.children[0]?.children[0];
    expect(row?.ask).toBe('About task Add retry (task-zz): ');
  });
});

describe('projects and schedules', () => {
  it('nests each project’s tasks under it, with a task count', () => {
    const nodes = buildExplorer(
      source({
        projects: keepValid([project()], projectRowSchema),
        tasks: [
          view({ id: 'task-1', project_id: 'project-a', title: 'One' }),
          view({ id: 'task-2', project_id: 'other', title: 'Two' })
        ]
      })
    );
    const projects = nodes.find((node) => node.id === 'projects');
    expect(projects?.count).toBe(1);
    expect(projects?.children[0]?.label).toBe('acme/widgets');
    expect(projects?.children[0]?.count).toBe(1);
    expect(projects?.children[0]?.children.map((child) => child.label)).toEqual(['One']);
  });

  it('shows a paused schedule as paused and not as a live one', () => {
    const nodes = buildExplorer(
      source({
        schedules: [
          schedule({ id: 'sch-1', config: { ...schedule().config, paused: true } }),
          schedule({ id: 'sch-2' })
        ]
      })
    );
    const schedules = nodes.find((node) => node.id === 'schedules');
    expect(schedules?.count).toBe(2);
    expect(schedules?.children[0]).toEqual(
      expect.objectContaining({ detail: 'Paused', tone: 'muted' })
    );
    expect(schedules?.children[1]?.tone).toBe('ok');
  });

  it('caps the finished group', () => {
    const tasks = Array.from({ length: FINISHED_LIMIT + 3 }, (_, index) =>
      view({ id: `done-${index}`, status: 'completed' })
    );
    const finished = buildExplorer(source({ tasks })).find((node) => node.id === 'finished');
    expect(finished?.count).toBe(FINISHED_LIMIT + 3);
    expect(finished?.children).toHaveLength(FINISHED_LIMIT);
  });
});

describe('visible rows', () => {
  it('lists only the rows under expanded nodes', () => {
    const nodes = buildExplorer(source({ attention: [hitlAttentionItem('req-3')] }));
    const folded = visibleRows(nodes, (id) => id === 'mainloop');
    expect(folded.some((row) => row.parentId === 'needs')).toBe(false);
    const open = visibleRows(nodes, (id) => id === 'needs');
    expect(open.find((row) => row.node.id === 'needs')?.depth).toBe(1);
    expect(open.find((row) => row.parentId === 'needs')?.depth).toBe(2);
  });
});
