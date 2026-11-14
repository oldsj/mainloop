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
import { projection, view } from '@/test/fixtures/tasks';
import {
  askPrefill,
  briefSnippet,
  buildExplorer,
  FINISHED_WINDOW_MS,
  heldUntilText,
  isHeld,
  mainloopState,
  needsCount,
  prStage,
  sameTarget,
  shortProject,
  targetOfLocation,
  taskNode,
  viewNodes,
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
    [1, 0, 'Needs you', 'attention'],
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
    // A schedule is at rest either way: its timing, or "Paused", is the line under its name.
    expect(schedules?.children[1]).toEqual(
      expect.objectContaining({ detail: schedule().config.schedule, tone: 'muted' })
    );
  });

  describe('finished and not yet seen', () => {
    const now = Date.parse('2026-01-10T00:00:00Z');
    const finishedAt = '2026-01-09T00:00:00Z';
    const before = Date.parse('2026-01-08T00:00:00Z');
    const after = Date.parse('2026-01-09T12:00:00Z');
    const toneOf = (status: 'completed' | 'cancelled' | 'failed', seen: ExplorerSource['seen']) => {
      const task = view({ id: 't', status, reason: null, updated_at: finishedAt });
      const finished = buildExplorer(source({ tasks: [task], now, seen })).find(
        (node) => node.id === 'finished'
      );
      return finished?.children[0]?.tone;
    };

    it('is teal until the owner has opened the task, then at rest', () => {
      expect(toneOf('completed', { since: before, seen: {} })).toBe('unseen');
      expect(toneOf('completed', { since: before, seen: { t: after } })).toBe('muted');
    });

    it('is unseen again when the task changes after it was opened', () => {
      expect(toneOf('completed', { since: before, seen: { t: before + 1 } })).toBe('unseen');
    });

    it('counts a task that finished before this browser kept a record as seen', () => {
      expect(toneOf('completed', { since: after, seen: {} })).toBe('muted');
    });

    it('lights nothing without a record, as when storage is blocked', () => {
      expect(toneOf('completed', null)).toBe('muted');
      expect(toneOf('completed', undefined)).toBe('muted');
    });

    it('leaves a cancelled task at rest and a failed one red, seen or not', () => {
      const seen = { since: before, seen: {} };
      const toneFor = (status: 'cancelled' | 'failed') =>
        taskNode(view({ id: 't', status, updated_at: finishedAt }), source({ seen }), 'x').tone;
      expect(toneFor('cancelled')).toBe('muted');
      expect(toneFor('failed')).toBe('bad');
    });
  });

  it('gives a waiting task the working tone, and never green to a row', () => {
    const waiting = view({ id: 'w', status: 'waiting' });
    const working = buildExplorer(source({ tasks: [waiting] })).find((n) => n.id === 'working');
    expect(working?.children[0]?.tone).toBe('active');
    const tones = visibleRows(buildExplorer(source({ tasks: [waiting] })), () => true)
      .filter((row) => row.node.id !== 'health')
      .map((row) => row.node.tone);
    expect(tones).not.toContain('ok');
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
  it('shows the title as the label, with the short project name and the time it last changed', () => {
    const projects = keepValid([project()], projectRowSchema);
    const task = view({
      id: 'task-1',
      project_id: 'project-a',
      title: 'Add retry button',
      brief: 'Add a retry button to the task page so a failed send can be sent again.'
    });
    const node = taskNode(task, source({ projects, tasks: [task] }), 'working');
    expect(node.label).toBe('Add retry button');
    expect(node.project).toBe('widgets');
    expect(node.at).toBe(task.task.updated_at);
    // The brief is an instruction to the agent, not a summary of the task: it is not on the row.
    expect(JSON.stringify(node)).not.toContain('so a failed send can be sent again');
  });

  it('falls back to a brief snippet, with no project, when the task has no title', () => {
    const brief = 'A legacy task created before a title was required on every delegated brief.';
    const task = view({ id: 'task-2', project_id: null, title: '', brief });
    const node = taskNode(task, source({ tasks: [task] }), 'working');
    expect(node.label).toBe(briefSnippet(brief, 60));
    expect(node.project).toBeUndefined();
  });

  it('keeps a long title whole on the node, for CSS to truncate', () => {
    const title = 'A'.repeat(200);
    const task = view({ id: 'task-3', project_id: null, title });
    const node = taskNode(task, source({ tasks: [task] }), 'working');
    expect(node.label).toBe(title);
  });
});

describe('shortProject', () => {
  it('drops the owner from a repository name', () => {
    expect(shortProject('acme/widgets')).toBe('widgets');
    expect(shortProject('widgets')).toBe('widgets');
    expect(shortProject('acme/')).toBe('acme/');
  });
});

describe('the row for the current page', () => {
  it.each([
    ['/', {}, { to: 'chat' }],
    ['/health', {}, { to: 'health' }],
    ['/inbox', {}, { to: 'needs', item: undefined }],
    ['/inbox', { item: 'queue:q1' }, { to: 'needs', item: 'queue:q1' }],
    ['/inbox', { show: 'problems', item: 'p1' }, { to: 'problems', item: 'p1' }],
    ['/tasks/t1', {}, { to: 'task', id: 't1' }],
    ['/tasks/t1/', {}, { to: 'task', id: 't1' }],
    ['/sessions/s1', {}, { to: 'session', id: 's1' }],
    ['/workspaces/w1', {}, { to: 'workspace', id: 'w1' }],
    ['/projects/p1', {}, { to: 'project', id: 'p1' }],
    ['/schedules/sc1', {}, { to: 'schedule', id: 'sc1' }]
  ])('maps %s %o to its target', (pathname, search, target) => {
    expect(targetOfLocation(pathname, search)).toEqual(target);
  });

  it.each([
    ['/inbox', { show: 'activity' }],
    ['/schedules', {}],
    ['/schedules/new', {}],
    ['/agents', {}],
    ['/nope', {}]
  ])('has no row for %s %o', (pathname, search) => {
    expect(targetOfLocation(pathname, search)).toBeNull();
  });

  it('matches a target by kind and by id or inbox item', () => {
    expect(sameTarget({ to: 'task', id: 't1' }, { to: 'task', id: 't1' })).toBe(true);
    expect(sameTarget({ to: 'task', id: 't1' }, { to: 'task', id: 't2' })).toBe(false);
    expect(sameTarget({ to: 'task', id: 't1' }, { to: 'session', id: 't1' })).toBe(false);
    expect(sameTarget({ to: 'chat' }, { to: 'chat' })).toBe(true);
    // The Needs you heading is the page with no item; a row is the page on its own item.
    expect(sameTarget({ to: 'needs' }, { to: 'needs', item: undefined })).toBe(true);
    expect(sameTarget({ to: 'needs' }, { to: 'needs', item: 'q1' })).toBe(false);
    expect(sameTarget({ to: 'needs', item: 'q1' }, { to: 'needs', item: 'q1' })).toBe(true);
    expect(sameTarget(undefined, { to: 'chat' })).toBe(false);
    expect(sameTarget({ to: 'chat' }, null)).toBe(false);
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

  it('counts a child needing the owner as needs you, per the attention items', () => {
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
    expect(node.detail).toBe('1/2 done · 1 needs you');
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
      ['1 task needs you', { to: 'needs', item: 'req-9' }],
      ['1 task failed', { to: 'task', id: 'w-1' }]
    ]);
    // Without a listed action for the task, the waiting row still opens the answer view.
    const bare = buildExplorer(source({ tasks: [stuck], lastVisit })).find(
      (node) => node.id === 'since'
    );
    expect(bare?.children[0]?.target).toEqual({ to: 'needs' });
  });
});

describe('groups by pull request state', () => {
  const open = { pr_state: 'open' as const, pr_number: 12 };
  const ready = (id = 'r') =>
    view(
      { id, status: 'waiting', title: 'Add retry' },
      { projection: projection({ ...open, ci_state: 'success' }) }
    );
  const landing = (id = 'l') =>
    view(
      { id, status: 'waiting', title: 'Ship docs' },
      { projection: projection({ ...open, ci_state: 'success', merge_proposal_id: 'mp-1' }) }
    );
  const group = (nodes: ReturnType<typeof buildExplorer>, id: string) =>
    nodes.find((node) => node.id === id);

  it.each([
    ['open, CI passing, no merge proposed', { ...open, ci_state: 'success' as const }, 'review'],
    ['open with a merge proposed', { ...open, merge_proposal_id: 'mp-1' }, 'landing'],
    [
      'open, merge proposed, CI unknown',
      { ...open, ci_state: 'unknown' as const, merge_proposal_id: 'mp-1' },
      'landing'
    ],
    [
      'merged, task still settling',
      { pr_state: 'merged' as const, merge_state: 'merged' },
      'landing'
    ],
    ['open, CI failing', { ...open, ci_state: 'failure' as const }, null],
    ['open, CI running', { ...open, ci_state: 'pending' as const }, null],
    ['open, CI not known for this head', { ...open, ci_state: 'unknown' as const }, null],
    ['closed without merging', { pr_state: 'closed' as const, ci_state: 'success' as const }, null],
    ['no pull request yet', {}, null]
  ])('reads %s', (_name, facts, stage) => {
    expect(prStage(view({ status: 'waiting' }, { projection: projection(facts) }))).toBe(stage);
  });

  it('moves a working task with a green open pull request to Ready for review', () => {
    const still = view({ id: 'w', status: 'running' });
    const nodes = buildExplorer(source({ tasks: [ready(), still] }));
    expect(group(nodes, 'working')?.children.map((n) => n.target)).toEqual([
      { to: 'task', id: 'w' }
    ]);
    expect(group(nodes, 'working')?.count).toBe(1);
    const review = group(nodes, 'review');
    expect(review).toEqual(expect.objectContaining({ label: 'Ready for review', count: 1 }));
    expect(review?.children[0]).toEqual(
      expect.objectContaining({
        label: 'Add retry',
        detail: 'PR #12 · CI passing',
        target: { to: 'task', id: 'r' }
      })
    );
  });

  it('shows a ready pull request as unseen until its page has been opened, then at rest', () => {
    const task = ready();
    const before = Date.parse(task.task.updated_at) - 1000;
    const toneWith = (seen: ExplorerSource['seen']) =>
      group(buildExplorer(source({ tasks: [task], seen })), 'review')?.children[0]?.tone;
    expect(toneWith({ since: before, seen: {} })).toBe('unseen');
    expect(toneWith({ since: before, seen: { r: Date.now() } })).toBe('muted');
    expect(toneWith(null)).toBe('muted');
  });

  it('puts a merge under way in Landing, as work still moving', () => {
    const merged = view(
      { id: 'm', status: 'running', title: 'Settle' },
      { projection: projection({ pr_state: 'merged', pr_number: 7, merge_state: 'merged' }) }
    );
    const nodes = buildExplorer(source({ tasks: [landing(), merged] }));
    const rows = group(nodes, 'landing')?.children ?? [];
    expect(group(nodes, 'landing')).toEqual(
      expect.objectContaining({ label: 'Landing', count: 2 })
    );
    expect(rows.map((n) => [n.detail, n.tone])).toEqual(
      expect.arrayContaining([
        ['PR #12 · merging', 'active'],
        ['PR #7 · merged, settling', 'active']
      ])
    );
    expect(group(nodes, 'working')?.count).toBe(0);
  });

  it('leaves both groups out when nothing is at either stage', () => {
    const nodes = buildExplorer(source({ tasks: [view({ id: 'w', status: 'running' })] }));
    expect(group(nodes, 'review')).toBeUndefined();
    expect(group(nodes, 'landing')).toBeUndefined();
  });

  it('keeps a task with an owner action under Needs you only', () => {
    const task = ready('n');
    const attention = [{ ...hitlAttentionItem('req-1'), task_id: 'n' }];
    const nodes = buildExplorer(source({ tasks: [task], attention }));
    expect(group(nodes, 'review')).toBeUndefined();
    expect(group(nodes, 'needs')?.count).toBe(1);
  });

  it('keeps a finished task out of both, whatever its pull request says', () => {
    const done = view(
      { id: 'd', status: 'completed', updated_at: new Date().toISOString() },
      { projection: projection({ pr_state: 'merged', merge_state: 'merged' }) }
    );
    const nodes = buildExplorer(source({ tasks: [done] }));
    expect(group(nodes, 'landing')).toBeUndefined();
    expect(group(nodes, 'finished')?.count).toBe(1);
  });

  it('counts a merge under way as Mainloop working, and a waiting review as idle', () => {
    expect(group(buildExplorer(source({ tasks: [landing()] })), 'mainloop')?.detail).toBe(
      'Working'
    );
    expect(group(buildExplorer(source({ tasks: [ready()] })), 'mainloop')?.detail).toBe('Idle');
  });

  it('sits after Working, opens Ready for review by default, and is in the Sessions view', () => {
    const nodes = buildExplorer(source({ tasks: [ready(), landing()] }));
    const ids = topLevel(nodes);
    expect(ids.slice(ids.indexOf('working'), ids.indexOf('working') + 3)).toEqual([
      'working',
      'review',
      'landing'
    ]);
    expect(isExpanded(DEFAULT_PREFS, 'review')).toBe(true);
    expect(isExpanded(DEFAULT_PREFS, 'landing')).toBe(false);
    expect(topLevel(viewNodes(nodes, 'sessions'))).toEqual(
      expect.arrayContaining(['review', 'landing'])
    );
    expect(topLevel(viewNodes(nodes, 'projects'))).toEqual(['projects', 'schedules']);
  });
});

describe('snoozed tasks', () => {
  const now = new Date(2026, 0, 10, 12, 0).getTime();
  const later = new Date(2026, 0, 10, 15, 40).toISOString();
  const tomorrow = new Date(2026, 0, 11, 9, 0).toISOString();
  const earlier = new Date(2026, 0, 10, 11, 0).toISOString();
  const group = (nodes: ReturnType<typeof buildExplorer>, id: string) =>
    nodes.find((node) => node.id === id);
  const time = (iso: string) =>
    new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

  it('reads a hold as held only until its time', () => {
    expect(isHeld(view({ held_until: later }), now)).toBe(true);
    expect(isHeld(view({ held_until: earlier }), now)).toBe(false);
    expect(isHeld(view({ held_until: null }), now)).toBe(false);
    expect(isHeld(view({ held_until: 'not a time' }), now)).toBe(false);
  });

  it('says when a hold ends: the time today, the weekday as well otherwise', () => {
    expect(heldUntilText(later, now)).toBe(`until ${time(later)}`);
    const weekday = new Date(tomorrow).toLocaleDateString([], { weekday: 'short' });
    expect(heldUntilText(tomorrow, now)).toBe(`until ${weekday} ${time(tomorrow)}`);
  });

  it('moves a held working task out of Working into Snoozed, at rest, with when and why', () => {
    const held = view({
      id: 'h',
      status: 'running',
      title: 'Wait for the vendor',
      held_until: later,
      held_reason: 'Vendor replies after lunch'
    });
    const busy = view({ id: 'w', status: 'running' });
    const nodes = buildExplorer(source({ tasks: [held, busy], now }));
    expect(group(nodes, 'working')?.children.map((n) => n.target)).toEqual([
      { to: 'task', id: 'w' }
    ]);
    const snoozed = group(nodes, 'snoozed');
    expect(snoozed).toEqual(expect.objectContaining({ label: 'Snoozed', count: 1 }));
    expect(snoozed?.children[0]).toEqual(
      expect.objectContaining({
        label: 'Wait for the vendor',
        detail: `until ${time(later)} · Vendor replies after lunch`,
        tone: 'muted',
        target: { to: 'task', id: 'h' }
      })
    );
    // The age is left out: the time that matters is when it comes back.
    expect(snoozed?.children[0]?.at).toBeUndefined();
  });

  it('keeps a held task out of Ready for review and Landing', () => {
    const green = { pr_state: 'open' as const, pr_number: 3, ci_state: 'success' as const };
    const ready = view(
      { id: 'r', status: 'waiting', held_until: later },
      { projection: projection(green) }
    );
    const landing = view(
      { id: 'l', status: 'waiting', held_until: later },
      { projection: projection({ ...green, merge_proposal_id: 'mp' }) }
    );
    const nodes = buildExplorer(source({ tasks: [ready, landing], now }));
    expect(group(nodes, 'review')).toBeUndefined();
    expect(group(nodes, 'landing')).toBeUndefined();
    expect(group(nodes, 'snoozed')?.count).toBe(2);
  });

  it('drops a held task from the Needs you rows read from task state', () => {
    const waiting = view({
      id: 'n',
      status: 'waiting',
      reason: 'owner_decision',
      held_until: later
    });
    const nodes = buildExplorer(source({ tasks: [waiting], now }));
    expect(group(nodes, 'needs')?.count).toBe(0);
    expect(group(nodes, 'snoozed')?.count).toBe(1);
  });

  it('still shows an owner action that /attention lists for a held task, under Needs you only', () => {
    const task = view({ id: 'a', status: 'running', held_until: later });
    const attention = [{ ...hitlAttentionItem('req-1'), task_id: 'a' }];
    const nodes = buildExplorer(source({ tasks: [task], attention, now }));
    expect(group(nodes, 'needs')?.count).toBe(1);
    expect(group(nodes, 'snoozed')).toBeUndefined();
  });

  it('brings the task back when the hold has run out', () => {
    const task = view({ id: 'b', status: 'running', held_until: earlier });
    const nodes = buildExplorer(source({ tasks: [task], now }));
    expect(group(nodes, 'working')?.count).toBe(1);
    expect(group(nodes, 'snoozed')).toBeUndefined();
  });

  it('does not count a held task as Mainloop working', () => {
    const task = view({ id: 'h', status: 'running', held_until: later });
    expect(group(buildExplorer(source({ tasks: [task], now })), 'mainloop')?.detail).toBe('Idle');
  });

  it('sits after Landing, folded, in the Sessions view, and is left out when empty', () => {
    const task = view({ id: 'h', status: 'running', held_until: later });
    const nodes = buildExplorer(source({ tasks: [task], now }));
    const ids = topLevel(nodes);
    expect(ids.indexOf('snoozed')).toBe(ids.indexOf('working') + 1);
    expect(ids.indexOf('snoozed')).toBeLessThan(ids.indexOf('projects'));
    expect(isExpanded(DEFAULT_PREFS, 'snoozed')).toBe(false);
    expect(topLevel(viewNodes(nodes, 'sessions'))).toContain('snoozed');
    expect(group(buildExplorer(source({ now })), 'snoozed')).toBeUndefined();
  });
});
