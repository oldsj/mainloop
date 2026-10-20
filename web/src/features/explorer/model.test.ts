import { describe, expect, it } from 'vitest';
import {
  attentionItem,
  hitlAttentionItem,
  problemAttentionItem,
  queueItem
} from '@/features/inbox/test/fixtures';
import { keepValid, projectRowSchema } from '@/features/projects/model';
import { project } from '@/features/projects/test/fixtures';
import { schedule } from '@/features/schedules/test/fixtures';
import { view } from '@/test/fixtures/tasks';
import {
  askPrefill,
  briefSnippet,
  buildExplorer,
  FINISHED_WINDOW_MS,
  mainloopState,
  needsCount,
  taskNode,
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
      'problems',
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

describe('problems', () => {
  it('counts a problem item and a blocked-on-recovery task once each, apart from Needs you', () => {
    const attention = [problemAttentionItem({ id: 'reconciliation:task-p', task_id: 'task-p' })];
    const tasks = [view({ id: 'task-stalled', status: 'blocked', reason: 'stalled' })];
    const nodes = buildExplorer(source({ attention, tasks }));
    const problems = nodes.find((node) => node.id === 'problems');
    expect(problems?.count).toBe(2);
    expect(needsCount(nodes)).toBe(0);
  });

  it('does not repeat a task that a problem item already names', () => {
    const tasks = [view({ id: 'task-p', status: 'blocked', reason: 'stalled' })];
    const attention = [problemAttentionItem({ id: 'reconciliation:task-p', task_id: 'task-p' })];
    const nodes = buildExplorer(source({ attention, tasks }));
    expect(nodes.find((node) => node.id === 'problems')?.count).toBe(1);
  });

  it('reads muted, not the attention yellow a real decision gets', () => {
    const attention = [problemAttentionItem({ id: 'reconciliation:task-p', task_id: 'task-p' })];
    const nodes = buildExplorer(source({ attention, tasks: [] }));
    const row = nodes.find((node) => node.id === 'problems')?.children[0];
    expect(row?.tone).toBe('muted');
  });

  it('is empty, apart from Needs you, with nothing for Mainloop to handle', () => {
    const nodes = buildExplorer(EMPTY);
    expect(nodes.find((node) => node.id === 'problems')?.count).toBe(0);
  });
});

const NO_ACTIONS = {
  retry: { available: false, reason: 'handoff_unavailable' },
  reassign: { available: false, reason: 'handoff_unavailable' },
  cancel: { available: false, reason: 'cancel_unavailable' }
} as const;

describe('failed tasks', () => {
  it('leaves a failed task with nothing left to do out of Needs you', () => {
    const stuck = view({ id: 'stuck', status: 'failed' }, { actions: NO_ACTIONS });
    const nodes = buildExplorer(source({ tasks: [stuck] }));
    expect(needsCount(nodes)).toBe(0);
  });

  it('keeps a failed task with an available action in Needs you', () => {
    const actionable = view(
      { id: 'actionable', status: 'failed' },
      { actions: { ...NO_ACTIONS, cancel: { available: true, reason: null } } }
    );
    const nodes = buildExplorer(source({ tasks: [actionable] }));
    expect(needsCount(nodes)).toBe(1);
  });

  it('lists a failed task with nothing left to do under Finished recently, within the window', () => {
    const now = Date.parse('2026-01-10T12:00:00Z');
    const stuck = view(
      { id: 'stuck', status: 'failed', updated_at: '2026-01-09T12:00:00Z' },
      { actions: NO_ACTIONS }
    );
    const nodes = buildExplorer(source({ tasks: [stuck], now }));
    const finished = nodes.find((node) => node.id === 'finished');
    expect(finished?.children.map((node) => node.target)).toEqual([{ to: 'task', id: 'stuck' }]);
  });

  it('drops a failed task with nothing left to do once it falls outside the finished window', () => {
    const now = Date.parse('2026-01-10T12:00:00Z');
    const old = view(
      {
        id: 'old',
        status: 'failed',
        updated_at: new Date(now - FINISHED_WINDOW_MS - 1).toISOString()
      },
      { actions: NO_ACTIONS }
    );
    const nodes = buildExplorer(source({ tasks: [old], now }));
    expect(needsCount(nodes)).toBe(0);
    expect(nodes.find((node) => node.id === 'finished')?.count).toBe(0);
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
    expect(askPrefill({ label: 'Insights' }, { to: 'health' })).toBeUndefined();
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

  it('lists only tasks finished in the last seven days, and counts the same rows', () => {
    const now = Date.parse('2026-01-10T12:00:00Z');
    const recent = view({ id: 'recent', status: 'completed', updated_at: '2026-01-09T12:00:00Z' });
    const old = view({
      id: 'old',
      status: 'cancelled',
      updated_at: new Date(now - FINISHED_WINDOW_MS - 1).toISOString()
    });
    const finished = buildExplorer(source({ tasks: [recent, old], now })).find(
      (node) => node.id === 'finished'
    );
    expect(finished?.count).toBe(1);
    expect(finished?.children.map((node) => node.target)).toEqual([{ to: 'task', id: 'recent' }]);
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

describe('needs you and working', () => {
  it('lists a running task with an owner action under Needs you only, not also under Working', () => {
    const running = view({ id: 'task-1', status: 'running', title: 'Pick queue backend' });
    const approval = attentionItem(queueItem({ item_type: 'approval', task_id: 'task-1' }));
    const nodes = buildExplorer(source({ tasks: [running], attention: [approval] }));
    const needs = nodes.find((node) => node.id === 'needs');
    const working = nodes.find((node) => node.id === 'working');
    expect(needs?.count).toBe(1);
    expect(working?.count).toBe(0);
    expect(working?.children).toEqual([expect.objectContaining({ note: true })]);
  });

  it('opens the answer view from the group and from each owner action, task or not', () => {
    const loose = attentionItem(
      queueItem({ id: 'q-loose', item_type: 'plan_review', task_id: null })
    );
    const request = hitlAttentionItem('req-1');
    const needs = buildExplorer(source({ attention: [loose, request] })).find(
      (node) => node.id === 'needs'
    );
    expect(needs?.target).toEqual({ to: 'needs' });
    expect(needs?.children.map((node) => node.target)).toEqual([
      { to: 'needs', item: 'q-loose' },
      { to: 'needs', item: 'req-1' }
    ]);
  });

  it('asks Mainloop about an owner action, naming its task when it has one', () => {
    const loose = attentionItem(queueItem({ title: 'Which branch?', task_id: null }));
    const tasked = attentionItem(
      queueItem({
        id: 'q-2',
        item_type: 'approval',
        title: 'Approve a push',
        task_id: 'task-1234abcd99'
      })
    );
    const rows = buildExplorer(source({ attention: [loose, tasked] })).find(
      (node) => node.id === 'needs'
    )?.children;
    expect(rows?.map((node) => node.ask)).toEqual([
      'About question Which branch?: ',
      'About approval Approve a push (task task-123): '
    ]);
  });
});

describe('brief snippet', () => {
  it('flattens whitespace and passes a short brief through unchanged', () => {
    expect(briefSnippet('Add a retry\nbutton   to the task page.')).toBe(
      'Add a retry button to the task page.'
    );
  });

  it('truncates a long brief to the given length with an ellipsis', () => {
    const brief = 'x'.repeat(200);
    const snippet = briefSnippet(brief, 140);
    expect(snippet).toHaveLength(141);
    expect(snippet.endsWith('…')).toBe(true);
    expect(snippet.slice(0, 140)).toBe('x'.repeat(140));
  });
});

describe('task row identity', () => {
  it('shows the title as the label, a brief snippet as the summary, and the project as a tag', () => {
    const projects = keepValid([project()], projectRowSchema);
    const task = view({
      id: 'task-1',
      project_id: 'project-a',
      title: 'Add retry button',
      brief: 'Add a retry button to the task page so a failed send can be sent again.'
    });
    const node = taskNode(task, source({ projects, tasks: [task] }), 'working');
    expect(node.label).toBe('Add retry button');
    expect(node.summary).toBe(
      'Add a retry button to the task page so a failed send can be sent again.'
    );
    expect(node.tag).toBe('acme/widgets');
  });

  it('falls back to a brief snippet, with no project tag, when the task has no title', () => {
    const brief = 'A legacy task created before a title was required on every delegated brief.';
    const task = view({ id: 'task-2', project_id: null, title: '', brief });
    const node = taskNode(task, source({ tasks: [task] }), 'working');
    expect(node.label).toBe(briefSnippet(brief, 60));
    expect(node.summary).toBeUndefined();
    expect(node.tag).toBeUndefined();
  });

  it('keeps a long title whole on the node, for CSS to truncate, and still shortens the summary', () => {
    const title = 'A'.repeat(200);
    const brief = `${'B'.repeat(200)} and some more detail after the cut.`;
    const task = view({ id: 'task-3', project_id: null, title, brief });
    const node = taskNode(task, source({ tasks: [task] }), 'working');
    expect(node.label).toBe(title);
    expect(node.summary).toHaveLength(141);
    expect(node.summary?.endsWith('…')).toBe(true);
  });
});

describe('parent task progress', () => {
  it('nests a child task under its parent, not also as a flat sibling in its project', () => {
    const projects = keepValid([project()], projectRowSchema);
    const parent = view({ id: 'parent-1', project_id: 'project-a', title: 'Ship feature' });
    const child = view({
      id: 'child-1',
      project_id: 'project-a',
      parent_task_id: 'parent-1',
      root_task_id: 'parent-1',
      title: 'Write tests'
    });
    const nodes = buildExplorer(source({ projects, tasks: [parent, child] }));
    const projectNode = nodes.find((node) => node.id === 'projects')?.children[0];
    expect(projectNode?.children.map((node) => node.label)).toEqual(['Ship feature']);
    expect(projectNode?.children[0]?.children.map((node) => node.label)).toEqual(['Write tests']);
  });

  it('removes a child from the flat Working list once it nests under its loaded parent', () => {
    const parent = view({ id: 'p', status: 'running', title: 'Parent' });
    const child = view({ id: 'c', status: 'running', parent_task_id: 'p', title: 'Child' });
    const nodes = buildExplorer(source({ tasks: [parent, child] }));
    const working = nodes.find((node) => node.id === 'working');
    expect(working?.children.map((node) => node.label)).toEqual(['Parent']);
    expect(working?.count).toBe(1);
  });

  it('counts all direct children done', () => {
    const parent = view({ id: 'p', title: 'Parent' });
    const children = [
      view({ id: 'c1', parent_task_id: 'p', root_task_id: 'p', status: 'completed' }),
      view({ id: 'c2', parent_task_id: 'p', root_task_id: 'p', status: 'cancelled' })
    ];
    const node = taskNode(parent, source({ tasks: [parent, ...children] }), 'working');
    expect(node.detail).toBe('2/2 done');
  });

  it('counts a mix of done, running and blocked children', () => {
    const parent = view({ id: 'p', title: 'Parent' });
    const children = [
      view({ id: 'c1', parent_task_id: 'p', root_task_id: 'p', status: 'completed' }),
      view({ id: 'c2', parent_task_id: 'p', root_task_id: 'p', status: 'completed' }),
      view({ id: 'c3', parent_task_id: 'p', root_task_id: 'p', status: 'completed' }),
      view({ id: 'c4', parent_task_id: 'p', root_task_id: 'p', status: 'running' }),
      view({ id: 'c5', parent_task_id: 'p', root_task_id: 'p', status: 'blocked' })
    ];
    const node = taskNode(parent, source({ tasks: [parent, ...children] }), 'working');
    expect(node.detail).toBe('3/5 done · 1 running · 1 blocked');
  });

  it('counts a blocked child without a running part', () => {
    const parent = view({ id: 'p', title: 'Parent' });
    const children = [
      view({ id: 'c1', parent_task_id: 'p', root_task_id: 'p', status: 'completed' }),
      view({ id: 'c2', parent_task_id: 'p', root_task_id: 'p', status: 'failed' })
    ];
    const node = taskNode(parent, source({ tasks: [parent, ...children] }), 'working');
    expect(node.detail).toBe('1/2 done · 1 blocked');
  });

  it('counts a child needing the owner as waiting on you, per the attention items', () => {
    const parent = view({ id: 'p', title: 'Parent' });
    const needsOwner = view({
      id: 'c1',
      parent_task_id: 'p',
      root_task_id: 'p',
      status: 'running'
    });
    const done = view({ id: 'c2', parent_task_id: 'p', root_task_id: 'p', status: 'completed' });
    const attention = [attentionItem(queueItem({ task_id: 'c1' }))];
    const node = taskNode(
      parent,
      source({ tasks: [parent, needsOwner, done], attention }),
      'working'
    );
    expect(node.detail).toBe('1/2 done · 1 waiting on you');
  });

  it('keeps a task with no children on its current status detail', () => {
    const parent = view({ id: 'p', title: 'Parent', status: 'running' });
    const node = taskNode(parent, source({ tasks: [parent] }), 'working');
    expect(node.detail).toBe('Running');
  });

  it("follows the progress with the parent's latest report, or else its newest child's", () => {
    const withOwnReport = view(
      { id: 'p1', title: 'Parent with its own report' },
      {
        reports: [
          {
            task_id: 'p1',
            attempt_id: 'a1',
            request_id: 'r1',
            summary: 'Older',
            outcome: 'progress',
            owner_action: null,
            evidence_refs: []
          },
          {
            task_id: 'p1',
            attempt_id: 'a1',
            request_id: 'r2',
            summary: 'Latest from the parent',
            outcome: 'progress',
            owner_action: null,
            evidence_refs: []
          }
        ]
      }
    );
    const child = view({ id: 'c1', parent_task_id: 'p1', root_task_id: 'p1', status: 'completed' });
    const node = taskNode(withOwnReport, source({ tasks: [withOwnReport, child] }), 'working');
    expect(node.detail).toBe('1/1 done · Latest from the parent');

    const parentNoReport = view({ id: 'p2', title: 'Parent' });
    const olderChild = view(
      {
        id: 'c2',
        parent_task_id: 'p2',
        root_task_id: 'p2',
        status: 'completed',
        updated_at: '2026-01-01T10:00:00Z'
      },
      {
        reports: [
          {
            task_id: 'c2',
            attempt_id: 'a2',
            request_id: 'r3',
            summary: 'Older child report',
            outcome: 'progress',
            owner_action: null,
            evidence_refs: []
          }
        ]
      }
    );
    const newerChild = view(
      {
        id: 'c3',
        parent_task_id: 'p2',
        root_task_id: 'p2',
        status: 'completed',
        updated_at: '2026-01-01T12:00:00Z'
      },
      {
        reports: [
          {
            task_id: 'c3',
            attempt_id: 'a3',
            request_id: 'r4',
            summary: 'Newest child report',
            outcome: 'progress',
            owner_action: null,
            evidence_refs: []
          }
        ]
      }
    );
    const node2 = taskNode(
      parentNoReport,
      source({ tasks: [parentNoReport, olderChild, newerChild] }),
      'working'
    );
    expect(node2.detail).toBe('2/2 done · Newest child report');
  });
});

describe('since you were here', () => {
  const lastVisit = Date.parse('2026-01-01T11:00:00Z');

  it('is left out with no baseline or when nothing changed', () => {
    const failed = view({ id: 'f-1', status: 'failed' });
    expect(topLevel(buildExplorer(source({ tasks: [failed] })))).not.toContain('since');
    const before = view({ id: 'f-2', status: 'failed', updated_at: '2026-01-01T10:00:00Z' });
    expect(topLevel(buildExplorer(source({ tasks: [before], lastVisit })))).not.toContain('since');
  });

  it('sits after Needs you, expanded, and each row opens the latest task it counts', () => {
    const failed = view({ id: 'f-1', status: 'failed', title: 'Fix CI flake' });
    const nodes = buildExplorer(source({ tasks: [failed], lastVisit }));
    expect(topLevel(nodes).slice(0, 4)).toEqual(['mainloop', 'needs', 'problems', 'since']);
    expect(isExpanded(DEFAULT_PREFS, 'since')).toBe(true);
    const since = nodes.find((node) => node.id === 'since');
    expect(since?.count).toBe(1);
    expect(since?.children).toEqual([
      expect.objectContaining({
        label: '1 task failed',
        detail: 'Fix CI flake',
        target: { to: 'task', id: 'f-1' }
      })
    ]);
  });

  it('opens the answer view from the waiting row and counts a task in two rows once', () => {
    const stuck = view({ id: 'w-1', status: 'failed', reason: 'owner_decision', title: 'Pick' });
    const attention = [{ ...hitlAttentionItem('req-9'), task_id: 'w-1' }];
    const since = buildExplorer(source({ tasks: [stuck], attention, lastVisit })).find(
      (node) => node.id === 'since'
    );
    expect(since?.count).toBe(1);
    expect(since?.children.map((node) => [node.label, node.target])).toEqual([
      ['1 task waiting on you', { to: 'needs', item: 'req-9' }],
      ['1 task failed', { to: 'task', id: 'w-1' }]
    ]);
    // Without a listed action for the task, the waiting row still opens the answer view.
    const bare = buildExplorer(source({ tasks: [stuck], lastVisit })).find(
      (node) => node.id === 'since'
    );
    expect(bare?.children[0]?.target).toEqual({ to: 'needs' });
  });
});
