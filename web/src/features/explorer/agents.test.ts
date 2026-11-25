import { describe, expect, it } from 'vitest';
import { attentionItem, queueItem } from '@/features/inbox/test/fixtures';
import { keepValid, projectRowSchema } from '@/features/projects/model';
import { project } from '@/features/projects/test/fixtures';
import { session } from '@/features/sessions/test/fixtures';
import { attempt, NOW, projection, view } from '@/test/fixtures/tasks';
import {
  buildAgentColumn,
  currentAgent,
  MAINLOOP_SPACE,
  newTaskPrefill,
  spaceOfPage,
  spaceTabs,
  workingCount
} from './agents';
import type { ExplorerSource } from './model';

const at = (minutesAgo: number) => new Date(NOW - minutesAgo * 60_000).toISOString();

const PROJECTS = keepValid(
  [
    project({ id: 'project-1', full_name: 'acme/widgets' }),
    project({ id: 'project-2', full_name: 'acme/infra' })
  ],
  projectRowSchema
);

function source(overrides: Partial<ExplorerSource> = {}): ExplorerSource {
  return {
    tasks: [],
    attention: [],
    sessions: [],
    workspaces: [],
    projects: PROJECTS,
    schedules: [],
    now: NOW,
    ...overrides
  };
}

const running = (id: string, rest: Parameters<typeof view>[0] = {}) =>
  view({ id, title: id, status: 'running', updated_at: at(5), ...rest });

describe('spaces', () => {
  it('lists the main thread’s space first, then each project by its short name', () => {
    // Given two projects and nothing running
    const { spaces } = buildAgentColumn(source());

    // Then Mainloop leads, and a project is named without its owner
    expect(spaces.map((space) => [space.id, space.label])).toEqual([
      [MAINLOOP_SPACE, 'main'],
      ['project-1', 'widgets'],
      ['project-2', 'infra']
    ]);
    expect(spaces[1]?.title).toBe('acme/widgets');
    expect(spaces[0]?.target).toEqual({ to: 'chat' });
    expect(spaces[1]?.target).toEqual({ to: 'project', id: 'project-1' });
  });

  it('says a space with no agents is idle, with a hollow dot', () => {
    const { spaces } = buildAgentColumn(source());

    expect(spaces.map((space) => [space.detail, space.tone])).toEqual([
      ['idle', 'muted'],
      ['idle', 'muted'],
      ['idle', 'muted']
    ]);
  });

  it('counts a space’s agents by what they are doing, and takes the most urgent dot', () => {
    // Given a project with two running tasks and one waiting on an approval
    const tasks = [
      running('a'),
      running('b'),
      view(
        { id: 'c', title: 'c', status: 'running', updated_at: at(1) },
        { projection: projection({ pending_approval_ids: ['request-1'] }) }
      )
    ];

    const { spaces } = buildAgentColumn(source({ tasks }));

    // Then the project says so, red over amber, and the other project stays idle
    expect(spaces[1]).toMatchObject({ detail: '1 needs you · 2 working', tone: 'attention' });
    expect(spaces[2]).toMatchObject({ detail: 'idle', tone: 'muted' });
  });

  it('says a space is not idle while Mainloop is recovering one of its tasks', () => {
    // Given a task whose session was lost: Mainloop's work, not the owner's
    const tasks = [
      view({ id: 'a', status: 'blocked', reason: 'session_lost', updated_at: at(5) }),
      running('b')
    ];

    const { spaces, agents } = buildAgentColumn(source({ tasks }));

    // Then its dot is at rest, and the space counts it as a problem
    expect(agents[1]).toMatchObject({ tone: 'muted', problem: true });
    expect(spaces[1]).toMatchObject({ detail: '1 working · 1 problem', tone: 'active' });
    expect(workingCount(agents)).toBe(1);
  });
});

describe('agents', () => {
  it('always lists the main thread first, in the Mainloop space', () => {
    const { agents } = buildAgentColumn(source());

    expect(agents).toHaveLength(1);
    expect(agents[0]).toMatchObject({
      id: 'main',
      space: MAINLOOP_SPACE,
      role: 'main',
      detail: 'Idle',
      tone: 'muted',
      target: { to: 'chat' }
    });
  });

  it('makes the main thread speak for the work it directs', () => {
    // Given one running task
    const working = buildAgentColumn(source({ tasks: [running('a')] }));
    // And then an owner action nothing else names
    const needed = buildAgentColumn(
      source({ tasks: [running('a')], attention: [attentionItem(queueItem({ task_id: null }))] })
    );

    expect(working.agents[0]).toMatchObject({ detail: 'Working', tone: 'active' });
    expect(needed.agents[0]).toMatchObject({ detail: 'Needs you', tone: 'attention' });
  });

  it('lists a task that has not ended under its project, titled by the task', () => {
    const { agents } = buildAgentColumn(
      source({ tasks: [running('a', { title: 'Add retry button', project_id: 'project-2' })] })
    );

    expect(agents[1]).toMatchObject({
      id: 'task:a',
      space: 'project-2',
      spaceLabel: 'infra',
      label: 'Add retry button',
      role: 'worker',
      detail: 'Running',
      tone: 'active'
    });
  });

  it('opens an agent that is still going on its conversation, and any other on its task', () => {
    // Given a running task with a session, one with no session yet, a failed one and a stuck one
    const tasks = [
      running('going'),
      view({ id: 'new', status: 'queued', current_attempt_id: null, updated_at: at(5) }),
      view({ id: 'ended', status: 'failed', updated_at: at(5) }),
      view({ id: 'lost', status: 'blocked', reason: 'session_lost', updated_at: at(5) })
    ];

    const { agents } = buildAgentColumn(source({ tasks }));
    const of = (id: string) => agents.find((agent) => agent.id === `task:${id}`);

    // Then the first opens where it can be read and told things, the others where their state is
    expect(of('going')?.target).toEqual({ to: 'session', id: 'session-1' });
    expect(of('new')?.target).toEqual({ to: 'task', id: 'new' });
    expect(of('ended')?.target).toEqual({ to: 'task', id: 'ended' });
    // A task Mainloop is recovering may have no session left to show
    expect(of('lost')?.target).toEqual({ to: 'task', id: 'lost' });
    // And either page of an agent is its own
    expect(of('going')?.pages).toEqual([
      { to: 'task', id: 'going' },
      { to: 'session', id: 'session-1' }
    ]);
  });

  it('groups agents by space in the spaces’ order, oldest first within a space', () => {
    // Given tasks created out of order across two projects
    const tasks = [
      running('later', { project_id: 'project-1', created_at: at(10) }),
      running('infra', { project_id: 'project-2', created_at: at(50) }),
      running('earlier', { project_id: 'project-1', created_at: at(30) })
    ];

    const { agents } = buildAgentColumn(source({ tasks }));

    // Then a row keeps its place as its state changes: the order is when it was started
    expect(agents.map((agent) => agent.id)).toEqual([
      'main',
      'task:earlier',
      'task:later',
      'task:infra'
    ]);
  });

  it('calls a coordination task a supervisor and lists the tasks it directs under it', () => {
    // Given a coordination task with two code tasks, one of them created before it was listed
    const tasks = [
      running('other', { created_at: at(90) }),
      running('child-2', { parent_task_id: 'lead', created_at: at(20) }),
      running('lead', { mode: 'coordination', created_at: at(60) }),
      running('child-1', { parent_task_id: 'lead', created_at: at(40) })
    ];

    const { agents } = buildAgentColumn(source({ tasks }));

    expect(agents.map((agent) => [agent.id, agent.role, agent.depth])).toEqual([
      ['main', 'main', 0],
      ['task:other', 'worker', 0],
      ['task:lead', 'supervisor', 0],
      ['task:child-1', 'worker', 1],
      ['task:child-2', 'worker', 1]
    ]);
    expect(agents[2]?.detail).toBe('Supervisor · 0/2 done · 2 running');
  });

  it('leaves out a finished task the owner has looked at, and keeps one they have not', () => {
    // Given two completed tasks, and a record that the owner opened only the first
    const seen = { since: NOW - 60 * 60_000, seen: { looked: NOW } };
    const tasks = [
      view({ id: 'looked', status: 'completed', updated_at: at(5) }),
      view({ id: 'fresh', status: 'completed', updated_at: at(5) }),
      view({ id: 'cancelled', status: 'cancelled', updated_at: at(5) })
    ];

    const { agents } = buildAgentColumn(source({ tasks, seen }));

    // Then only the unseen one is an agent, with the teal dot
    expect(agents.map((agent) => agent.id)).toEqual(['main', 'task:fresh']);
    expect(agents[1]?.tone).toBe('unseen');
  });

  it('keeps a finished task listed while its page, or its session’s, is the one showing', () => {
    // Given a completed task the owner has looked at, whose session is session-1
    const seen = { since: NOW - 60 * 60_000, seen: { looked: NOW } };
    const tasks = [view({ id: 'looked', status: 'completed', updated_at: at(5) })];
    const src = source({ tasks, seen });

    // Then it is an agent on its own pages only
    const ids = (page: Parameters<typeof buildAgentColumn>[1]) =>
      buildAgentColumn(src, page).agents.map((agent) => agent.id);
    expect(ids({ to: 'task', id: 'looked' })).toEqual(['main', 'task:looked']);
    expect(ids({ to: 'session', id: 'session-1' })).toEqual(['main', 'task:looked']);
    expect(ids({ to: 'chat' })).toEqual(['main']);
  });

  it('says what the owner is asked, in red, on a task an owner action names', () => {
    const tasks = [running('a')];
    const attention = [attentionItem(queueItem({ task_id: 'a', content: 'Approve the deploy' }))];

    const { agents } = buildAgentColumn(source({ tasks, attention }));

    expect(agents[1]).toMatchObject({ detail: 'Approve the deploy', tone: 'attention' });
  });

  it('says where a pull request stands on a task that only waits on review', () => {
    const tasks = [
      view(
        { id: 'a', status: 'waiting', reason: null, updated_at: at(5) },
        {
          projection: projection({
            pr_state: 'open',
            pr_number: 12,
            ci_state: 'success',
            pr_head_sha: 'abc',
            ci_head_sha: 'abc'
          })
        }
      )
    ];

    const { agents } = buildAgentColumn(source({ tasks }));

    expect(agents[1]?.detail).toBe('PR #12 · CI passing');
  });

  it('says the same of a pull request that waits on the owner, in red', () => {
    // Given a task waiting on publication with an open, green pull request: only the owner moves it
    const tasks = [
      view(
        { id: 'a', status: 'waiting', reason: 'publication', updated_at: at(5) },
        { projection: projection({ pr_state: 'open', pr_number: 228, ci_state: 'success' }) }
      )
    ];

    const { agents } = buildAgentColumn(source({ tasks }));

    expect(agents[1]).toMatchObject({ detail: 'PR #228 · CI passing', tone: 'attention' });
  });

  it('lists a live session that belongs to no task, in its project or else under Mainloop', () => {
    // Given a session of a task, one with a project and no task, and one with neither
    const tasks = [running('a', {})];
    const sessions = [
      session({ id: 'session-1', title: 'Task session', status: 'active' }),
      session({ id: 'loose', title: 'Canary', status: 'active', project_id: 'project-2' }),
      session({ id: 'free', title: 'Scratch', status: 'active', project_id: null }),
      session({ id: 'done', title: 'Old', status: 'completed', project_id: null })
    ];

    const { agents } = buildAgentColumn(source({ tasks, sessions }));

    // Then the task's session is not listed twice, and a finished session is not listed
    expect(agents.map((agent) => [agent.id, agent.space])).toEqual([
      ['main', MAINLOOP_SPACE],
      ['session:free', MAINLOOP_SPACE],
      ['task:a', 'project-1'],
      ['session:loose', 'project-2']
    ]);
    expect(agents[1]).toMatchObject({ role: 'session', target: { to: 'session', id: 'free' } });
  });

  it('puts a task whose project is not loaded under Mainloop', () => {
    const { agents, spaces } = buildAgentColumn(
      source({ tasks: [running('a', { project_id: 'gone' })] })
    );

    expect(agents[1]?.space).toBe(MAINLOOP_SPACE);
    expect(spaces[0]?.detail).toBe('1 working');
  });
});

describe('a project’s standing supervisor', () => {
  const supervisor = (rest: Parameters<typeof view>[0] = {}) =>
    view(
      {
        id: 'sup',
        title: 'Supervisor of acme/widgets',
        mode: 'coordination',
        standing: true,
        status: 'waiting',
        reason: 'idle',
        created_at: at(30),
        updated_at: at(600),
        ...rest
      },
      { attempts: [attempt({ task_id: 'sup', session_id: 'session-sup' })] }
    );

  it('is always an agent, first in its space, named for what it is, hollow at rest', () => {
    // Given a supervisor at rest, started after a task that is still running
    const tasks = [running('older', { created_at: at(90) }), supervisor()];

    const { agents, spaces } = buildAgentColumn(source({ tasks }));

    // Then it leads its space, and being at rest is neither work nor anything for the owner
    expect(agents.map((agent) => agent.id)).toEqual(['main', 'task:sup', 'task:older']);
    expect(agents[1]).toMatchObject({
      label: 'Supervisor',
      role: 'supervisor',
      standing: true,
      detail: 'Idle',
      tone: 'muted',
      problem: false,
      target: { to: 'session', id: 'session-sup' }
    });
    expect(workingCount(agents)).toBe(1);
    expect(spaces[1]).toMatchObject({ detail: '1 working', tone: 'active' });
    expect(agents[0]).toMatchObject({ detail: 'Working' });
  });

  it('leaves a space with only a supervisor at rest idle', () => {
    const { agents, spaces } = buildAgentColumn(source({ tasks: [supervisor()] }));

    expect(spaces[1]).toMatchObject({ detail: 'idle', tone: 'muted' });
    expect(agents[0]).toMatchObject({ detail: 'Idle', tone: 'muted' });
  });

  it('is amber while it works, with the workers it directs listed under it', () => {
    const tasks = [
      supervisor({ status: 'running', reason: null }),
      running('worker', { parent_task_id: 'sup' })
    ];

    const { agents } = buildAgentColumn(source({ tasks }));

    expect(agents.slice(1).map((agent) => [agent.id, agent.depth, agent.tone])).toEqual([
      ['task:sup', 0, 'active'],
      ['task:worker', 1, 'active']
    ]);
    expect(agents[1]?.detail).toBe('0/1 done · 1 running');
  });

  it('is the space’s first tab, and the project page is a tab only while it shows', () => {
    const tasks = [supervisor(), running('worker', { parent_task_id: 'sup' })];
    const src = source({ tasks });
    const strip = (page: Parameters<typeof spaceTabs>[2], pathname: string) =>
      spaceTabs(buildAgentColumn(src, page), 'project-1', page, pathname).map(
        (tab) => `${tab.label}${tab.current ? '*' : ''}`
      );

    // On the supervisor's conversation: supervisor, then its workers, and no overview tab
    expect(strip({ to: 'session', id: 'session-sup' }, '/sessions/session-sup')).toEqual([
      'Supervisor*',
      'worker'
    ]);
    // On the project's page, reached from the space's row: a last tab of its own
    expect(strip({ to: 'project', id: 'project-1' }, '/projects/project-1')).toEqual([
      'Supervisor',
      'worker',
      'Overview*'
    ]);
  });

  it('keeps overview first in a project that has no standing supervisor yet', () => {
    const src = source({ tasks: [running('worker')] });
    const page = { to: 'project', id: 'project-1' } as const;

    expect(
      spaceTabs(buildAgentColumn(src, page), 'project-1', page, '/projects/project-1').map(
        (tab) => tab.label
      )
    ).toEqual(['Overview', 'worker']);
  });
});

describe('the page being shown', () => {
  const tasks = [
    view(
      { id: 'a', status: 'running', project_id: 'project-2', updated_at: at(5) },
      { attempts: [attempt({ task_id: 'a', session_id: 'session-a' })] }
    )
  ];
  const sessions = [session({ id: 'loose', status: 'active', project_id: 'project-1' })];
  const src = source({ tasks, sessions });

  it('marks an agent on its task page and on its session', () => {
    const { agents } = buildAgentColumn(src);

    expect(currentAgent(agents, { to: 'task', id: 'a' })?.id).toBe('task:a');
    expect(currentAgent(agents, { to: 'session', id: 'session-a' })?.id).toBe('task:a');
    expect(currentAgent(agents, { to: 'chat' })?.id).toBe('main');
    expect(currentAgent(agents, { to: 'health' })).toBeUndefined();
    expect(currentAgent(agents, null)).toBeUndefined();
  });

  it('finds the space of a project, a task, a session and of a page that has none', () => {
    expect(spaceOfPage({ to: 'project', id: 'project-1' }, src)).toBe('project-1');
    expect(spaceOfPage({ to: 'task', id: 'a' }, src)).toBe('project-2');
    expect(spaceOfPage({ to: 'session', id: 'session-a' }, src)).toBe('project-2');
    expect(spaceOfPage({ to: 'session', id: 'loose' }, src)).toBe('project-1');
    expect(spaceOfPage({ to: 'chat' }, src)).toBe(MAINLOOP_SPACE);
    expect(spaceOfPage({ to: 'health' }, src)).toBe(MAINLOOP_SPACE);
    expect(spaceOfPage({ to: 'task', id: 'unknown' }, src)).toBe(MAINLOOP_SPACE);
    expect(spaceOfPage(null, src)).toBe(MAINLOOP_SPACE);
  });
});

describe('the tab strip', () => {
  const tasks = [
    view(
      { id: 'a', title: 'Add retry button', status: 'running', updated_at: at(5) },
      { attempts: [attempt({ task_id: 'a', session_id: 'session-a' })] }
    ),
    view({ id: 'b', title: 'Fix CI flake', status: 'running', updated_at: at(5) }),
    view({ id: 'c', title: 'Infra work', status: 'running', project_id: 'project-2' })
  ];
  const sessions = [session({ id: 'free', title: 'Scratch', status: 'active', project_id: null })];
  const src = source({ tasks, sessions });
  const strip = (page: Parameters<typeof spaceTabs>[2], pathname: string) => {
    const model = buildAgentColumn(src, page);
    return spaceTabs(model, spaceOfPage(page, src), page, pathname).map(
      (tab) => `${tab.label}${tab.current ? '*' : ''}`
    );
  };

  it('is the main chat and the agents of no project, in the main thread’s space', () => {
    expect(strip({ to: 'chat' }, '/')).toEqual(['Mainloop*', 'Scratch']);
  });

  it('is the project’s overview, then its agents, with the one being shown lit', () => {
    expect(strip({ to: 'project', id: 'project-1' }, '/projects/project-1')).toEqual([
      'Overview*',
      'Add retry button',
      'Fix CI flake'
    ]);
    expect(strip({ to: 'task', id: 'b' }, '/tasks/b')).toEqual([
      'Overview',
      'Add retry button',
      'Fix CI flake*'
    ]);
    // An agent's tab is lit on its session as on its task page.
    expect(strip({ to: 'session', id: 'session-a' }, '/sessions/session-a')).toEqual([
      'Overview',
      'Add retry button*',
      'Fix CI flake'
    ]);
  });

  it('gives a page that is no agent’s a last tab of its own, so one tab is always lit', () => {
    expect(strip({ to: 'needs' }, '/inbox')).toEqual(['Mainloop', 'Scratch', 'Inbox*']);
    expect(strip(null, '/settings')).toEqual(['Mainloop', 'Scratch', 'Settings*']);
    expect(strip(null, '/schedules/new')).toEqual(['Mainloop', 'Scratch', 'Schedules*']);
  });

  it('leads each tab to its page, except the one that only names the page being shown', () => {
    const page = { to: 'needs' } as const;
    const tabs = spaceTabs(buildAgentColumn(src, page), MAINLOOP_SPACE, page, '/inbox');

    expect(tabs.map((tab) => tab.target)).toEqual([
      { to: 'chat' },
      { to: 'session', id: 'free' },
      undefined
    ]);
  });

  it('starts new work in the project from the chat, by name', () => {
    const model = buildAgentColumn(src);

    expect(newTaskPrefill(model, 'project-1')).toBe('Start a task in acme/widgets: ');
    expect(newTaskPrefill(model, MAINLOOP_SPACE)).toBeUndefined();
  });
});
