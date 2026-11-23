import { describe, expect, it } from 'vitest';
import { attentionItem, queueItem } from '@/features/inbox/test/fixtures';
import { keepValid, projectRowSchema } from '@/features/projects/model';
import { project } from '@/features/projects/test/fixtures';
import { projection, view } from '@/test/fixtures/tasks';
import {
  askPrefill,
  briefSnippet,
  heldUntilText,
  isHeld,
  mainloopState,
  prStage,
  sameTarget,
  shortProject,
  stagedTaskNode,
  targetOfLocation,
  taskNode,
  type ExplorerSource
} from './model';

const EMPTY: ExplorerSource = {
  tasks: [],
  attention: [],
  sessions: [],
  workspaces: [],
  projects: [],
  schedules: []
};

function source(overrides: Partial<ExplorerSource> = {}): ExplorerSource {
  return { ...EMPTY, ...overrides };
}

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

  it('puts the ask on a task row', () => {
    const task = view({ id: 'task-zz', project_id: 'project-a', title: 'Add retry' });
    const row = taskNode(task, source({ tasks: [task] }), 'x');
    expect(row.ask).toBe('About task Add retry (task-zz): ');
  });
});

describe('a task row’s dot', () => {
  const finishedAt = '2026-01-09T00:00:00Z';
  const before = Date.parse('2026-01-08T00:00:00Z');
  const after = Date.parse('2026-01-09T12:00:00Z');
  const toneOf = (status: 'completed' | 'cancelled' | 'failed', seen: ExplorerSource['seen']) =>
    taskNode(view({ id: 't', status, reason: null, updated_at: finishedAt }), source({ seen }), 'x')
      .tone;

  it('is teal on a finished task until the owner has opened it, then at rest', () => {
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
    expect(toneOf('cancelled', seen)).toBe('muted');
    expect(toneOf('failed', seen)).toBe('bad');
  });

  it('gives a waiting task the working tone, and never green to a row', () => {
    const statuses = [
      'queued',
      'running',
      'waiting',
      'blocked',
      'failed',
      'completed',
      'cancelled'
    ] as const;
    const toneFor = (status: (typeof statuses)[number]) =>
      taskNode(view({ id: 'w', status }), source(), 'x').tone;
    expect(toneFor('waiting')).toBe('active');
    expect(statuses.map(toneFor)).not.toContain('ok');
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
            evidence_refs: [],
            screenshot_ids: []
          },
          {
            task_id: 'p1',
            attempt_id: 'a1',
            request_id: 'r2',
            summary: 'Latest from the parent',
            outcome: 'progress',
            owner_action: null,
            evidence_refs: [],
            screenshot_ids: []
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
            evidence_refs: [],
            screenshot_ids: []
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
            evidence_refs: [],
            screenshot_ids: []
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

describe('pull request stage', () => {
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
  const row = (task: ReturnType<typeof view>, seen?: ExplorerSource['seen']) =>
    stagedTaskNode(task, source({ tasks: [task], seen }), 'x', Date.now());

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

  it('reads a working task with a green open pull request by its pull request', () => {
    expect(row(ready())).toEqual(
      expect.objectContaining({
        label: 'Add retry',
        detail: 'PR #12 · CI passing',
        target: { to: 'task', id: 'r' }
      })
    );
    // One whose CI has not passed is still work in progress, and reads by its status.
    expect(row(view({ id: 'w', status: 'running' })).detail).toBe('Running');
  });

  it('shows a ready pull request as unseen until its page has been opened, then at rest', () => {
    const task = ready();
    const before = Date.parse(task.task.updated_at) - 1000;
    const toneWith = (seen: ExplorerSource['seen']) => row(task, seen).tone;
    expect(toneWith({ since: before, seen: {} })).toBe('unseen');
    expect(toneWith({ since: before, seen: { r: Date.now() } })).toBe('muted');
    expect(toneWith(null)).toBe('muted');
  });

  it('reads a merge under way as work still moving', () => {
    const merged = view(
      { id: 'm', status: 'running', title: 'Settle' },
      { projection: projection({ pr_state: 'merged', pr_number: 7, merge_state: 'merged' }) }
    );
    expect([landing(), merged].map((task) => [row(task).detail, row(task).tone])).toEqual([
      ['PR #12 · merging', 'active'],
      ['PR #7 · merged, settling', 'active']
    ]);
  });

  it('reads a finished task by its status, whatever its pull request says', () => {
    const done = view(
      { id: 'd', status: 'completed', updated_at: new Date().toISOString() },
      { projection: projection({ pr_state: 'merged', pr_number: 7, merge_state: 'merged' }) }
    );
    expect(row(done)).toEqual(expect.objectContaining({ detail: 'Completed', tone: 'muted' }));
  });
});

describe('snoozed tasks', () => {
  const now = new Date(2026, 0, 10, 12, 0).getTime();
  const later = new Date(2026, 0, 10, 15, 40).toISOString();
  const tomorrow = new Date(2026, 0, 11, 9, 0).toISOString();
  const earlier = new Date(2026, 0, 10, 11, 0).toISOString();
  const time = (iso: string) =>
    new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  const row = (task: ReturnType<typeof view>) =>
    stagedTaskNode(task, source({ tasks: [task], now }), 'x', now);

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

  it('reads a held working task at rest, with when it comes back and why', () => {
    const held = view({
      id: 'h',
      status: 'running',
      title: 'Wait for the vendor',
      updated_at: new Date(now - 60_000).toISOString(),
      held_until: later,
      held_reason: 'Vendor replies after lunch'
    });
    expect(row(held)).toEqual(
      expect.objectContaining({
        label: 'Wait for the vendor',
        detail: `until ${time(later)} · Vendor replies after lunch`,
        tone: 'muted',
        target: { to: 'task', id: 'h' }
      })
    );
    // The age is left out: the time that matters is when it comes back.
    expect(row(held).at).toBeUndefined();
    // With no reason given, the line is the time alone.
    expect(row(view({ id: 'h', status: 'running', held_until: later })).detail).toBe(
      `until ${time(later)}`
    );
  });

  it('reads a held task by its hold, not by its pull request', () => {
    const green = { pr_state: 'open' as const, pr_number: 3, ci_state: 'success' as const };
    const ready = view(
      { id: 'r', status: 'waiting', held_until: later },
      { projection: projection(green) }
    );
    const landing = view(
      { id: 'l', status: 'waiting', held_until: later },
      { projection: projection({ ...green, merge_proposal_id: 'mp' }) }
    );
    expect([ready, landing].map((task) => [row(task).detail, row(task).tone])).toEqual([
      [`until ${time(later)}`, 'muted'],
      [`until ${time(later)}`, 'muted']
    ]);
  });

  it('reads a held task that waits on the owner by its hold, at rest', () => {
    const waiting = view({
      id: 'n',
      status: 'waiting',
      reason: 'owner_decision',
      held_until: later
    });
    expect(row(waiting)).toEqual(
      expect.objectContaining({ detail: `until ${time(later)}`, tone: 'muted' })
    );
  });

  it('reads as usual again once the hold has run out', () => {
    const task = view({ id: 'b', status: 'running', held_until: earlier });
    expect(row(task)).toEqual(
      expect.objectContaining({ detail: 'Running', tone: 'active', at: task.task.updated_at })
    );
  });
});
