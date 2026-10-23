import { act, screen, within } from '@testing-library/react';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection, keys } from '@/api';
import { reasonLabel } from '@/lib/taskState';
import { attempt, projection, view } from '@/test/fixtures/tasks';
import { server } from '@/test/msw/server';
import { renderTasks } from './test/render';
import { problem, providersRead, tasksList } from './test/handlers';
import { TaskTree } from './TaskTree';

afterEach(async () => {
  // A lost reply marks the shared connection offline, which would leak into the next test.
  await connection.check();
});

// Reads skip the app's rate limiter here; see fastReads.ts for why this mock sits in each test file.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('./test/fastReads');
  return withFastReads(await importOriginal());
});

function deferred() {
  let release!: () => void;
  const promise = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}

it('shows the current attempt model and effort in the task list', async () => {
  server.use(
    tasksList(() =>
      HttpResponse.json([
        view({}, { attempts: [attempt({ model: 'claude-opus-5', effort: 'high' })] })
      ])
    ),
    providersRead()
  );
  renderTasks('/', () => <TaskTree />);

  expect(await screen.findByRole('link', { name: /Add retry button/ })).toHaveTextContent(
    'claude · claude-opus-5 · high effort'
  );
});

/**
 * Port of the frozen CI matrix (`frontend/src/lib/taskRecovery.test.ts`, test 2). The list shows CI
 * only when the PR head, the CI head and the observation all agree; every other shape reads unknown.
 */
const CI_STATES = ['success', 'failure', 'pending'] as const;
const EVIDENCE = [
  { label: 'no evidence', evidence: {} },
  { label: 'PR head only, fresh', evidence: { pr_head_sha: 'aaa', observed_at: 'fresh' } },
  { label: 'CI head only, fresh', evidence: { ci_head_sha: 'aaa', observed_at: 'fresh' } },
  {
    label: 'matching heads without an observation',
    evidence: { pr_head_sha: 'aaa', ci_head_sha: 'aaa' }
  },
  {
    label: 'mismatched heads, fresh',
    evidence: { pr_head_sha: 'aaa', ci_head_sha: 'bbb', observed_at: 'fresh' }
  },
  {
    label: 'an invalid observation time',
    evidence: { pr_head_sha: 'aaa', ci_head_sha: 'aaa', observed_at: 'invalid' }
  },
  {
    label: 'a 2020 observation',
    evidence: { pr_head_sha: 'aaa', ci_head_sha: 'aaa', observed_at: '2020-01-01T00:00:00Z' }
  }
];
const EXPECTED_WORD = { success: 'passing', failure: 'failing', pending: 'running' } as const;

type Evidence = { pr_head_sha?: string; ci_head_sha?: string; observed_at?: string };

/** Replaces the frozen `fresh` placeholder with the current time, as the frozen test does. */
function evidenceNow(evidence: Evidence): Evidence {
  if (evidence.observed_at !== 'fresh') return evidence;
  return { ...evidence, observed_at: new Date().toISOString() };
}

describe('CI on the task list', () => {
  for (const ci_state of CI_STATES) {
    for (const { label, evidence } of EVIDENCE) {
      it(`shows CI unknown for ${ci_state} with ${label}`, async () => {
        server.use(
          tasksList(() =>
            HttpResponse.json([
              view({}, { projection: projection({ ci_state, ...evidenceNow(evidence) }) })
            ])
          ),
          providersRead()
        );
        renderTasks('/', () => <TaskTree />);

        expect(await screen.findByText('CI unknown')).toBeInTheDocument();
        expect(screen.queryByText(/^CI (passing|failing|running)$/)).not.toBeInTheDocument();
      });
    }

    it(`shows CI ${EXPECTED_WORD[ci_state]} for ${ci_state} with fresh matching evidence`, async () => {
      server.use(
        tasksList(() =>
          HttpResponse.json([
            view(
              {},
              {
                projection: projection({
                  ci_state,
                  pr_head_sha: 'aaa',
                  ci_head_sha: 'aaa',
                  observed_at: new Date().toISOString()
                })
              }
            )
          ])
        ),
        providersRead()
      );
      renderTasks('/', () => <TaskTree />);

      expect(await screen.findByText(`CI ${EXPECTED_WORD[ci_state]}`)).toBeInTheDocument();
    });
  }
});

describe('task tree', () => {
  it('nests a child under its parent and indents it one step', async () => {
    const parent = view({ id: 'task-parent', title: 'Parent work' });
    const child = view({ id: 'task-child', title: 'Child work', parent_task_id: 'task-parent' });
    server.use(
      tasksList(() => HttpResponse.json([parent, child])),
      providersRead()
    );
    renderTasks('/', () => <TaskTree />);

    const row = await screen.findByRole('link', { name: /Child work/ });
    expect(row.closest('li')).toHaveClass('pl-md');
    expect(screen.getByRole('link', { name: /Parent work/ }).closest('li')).not.toHaveClass(
      'pl-md'
    );
    expect(row).toHaveClass('min-h-11');
    expect(row).toHaveAttribute('href', '/tasks/task-child');
  });

  it('says a child whose parent is not in the list is detached', async () => {
    const orphan = view({
      id: 'task-orphan',
      title: 'Orphan work',
      parent_task_id: 'task-missing'
    });
    server.use(
      tasksList(() => HttpResponse.json([orphan])),
      providersRead()
    );
    renderTasks('/', () => <TaskTree />);

    const row = await screen.findByRole('link', { name: /Orphan work/ });
    expect(within(row).getByText('child of a task not shown here')).toBeInTheDocument();
    expect(row.closest('li')).not.toHaveClass('pl-md');
  });

  it('marks a supervisor task and names the reason it is waiting', async () => {
    const supervisor = view(
      { id: 'task-sup', title: 'Supervise', mode: 'coordination', reason: 'owner_decision' },
      {}
    );
    server.use(
      tasksList(() => HttpResponse.json([supervisor])),
      providersRead()
    );
    renderTasks('/', () => <TaskTree />);

    const row = await screen.findByRole('link', { name: /Supervise/ });
    expect(within(row).getByText('[SUPERVISOR]')).toBeInTheDocument();
    expect(
      within(row).getByText(new RegExp(reasonLabel('owner_decision') as string))
    ).toBeInTheDocument();
  });

  it('does not mark a cancelled supervisor task as active', async () => {
    const cancelled = view(
      { id: 'task-sup-done', title: 'Supervise done', mode: 'coordination', status: 'cancelled' },
      {}
    );
    server.use(
      tasksList(() => HttpResponse.json([cancelled])),
      providersRead()
    );
    renderTasks('/', () => <TaskTree />);

    const row = await screen.findByRole('link', { name: /Supervise done/ });
    const badge = within(row).getByText('[SUPERVISOR]');
    expect(badge).toHaveClass('text-term-fg-muted');
    expect(badge).not.toHaveClass('text-term-cyan');
  });

  it('says there are no tasks when the list is empty', async () => {
    server.use(
      tasksList(() => HttpResponse.json([])),
      providersRead()
    );
    renderTasks('/', () => <TaskTree />);

    expect(await screen.findByText('No tasks yet.')).toBeInTheDocument();
  });

  it('reports loading until the first list arrives', async () => {
    const gate = deferred();
    server.use(
      tasksList(async () => {
        await gate.promise;
        return HttpResponse.json([]);
      }),
      providersRead()
    );
    renderTasks('/', () => <TaskTree />);

    expect(await screen.findByRole('status')).toHaveTextContent('Loading tasks…');
    gate.release();
    expect(await screen.findByText('No tasks yet.')).toBeInTheDocument();
  });

  it('shows an error and no rows when the first list fails', async () => {
    server.use(tasksList(problem(500, 'list offline')), providersRead());
    renderTasks('/', () => <TaskTree />);

    expect(await screen.findByRole('alert')).toHaveTextContent('Could not load tasks:');
    expect(screen.queryByTestId('task-list')).not.toBeInTheDocument();
    expect(screen.queryByText('No tasks yet.')).not.toBeInTheDocument();
  });

  it('keeps the rows and says so when a later list fails', async () => {
    let down = false;
    server.use(
      tasksList(() =>
        down
          ? HttpResponse.json({ detail: 'list offline' }, { status: 500 })
          : HttpResponse.json([view({ id: 'task-1', title: 'Still here' })])
      ),
      providersRead()
    );
    const { queryClient } = renderTasks('/', () => <TaskTree />);
    await screen.findByText('Still here');

    down = true;
    await act(() => queryClient.invalidateQueries({ queryKey: keys.tasks() }));

    expect(await screen.findByRole('alert')).toHaveTextContent('Showing the last known state.');
    expect(screen.getByText('Still here')).toBeInTheDocument();
  });

  it('asks the server for one project when a project is given', async () => {
    const seen: (string | null)[] = [];
    server.use(
      tasksList(({ request }) => {
        seen.push(new URL(request.url).searchParams.get('project_id'));
        return HttpResponse.json([]);
      }),
      providersRead()
    );
    renderTasks('/', () => <TaskTree projectId="project-1" />);

    await screen.findByText('No tasks yet.');
    expect(seen).toEqual(['project-1']);
  });
});

describe('task rows this page cannot read', () => {
  it('shows the readable rows and says how many it could not show', async () => {
    server.use(
      tasksList(() =>
        HttpResponse.json([
          view({ id: 'task-1', title: 'Readable work' }),
          { task: { id: 'broken' } }
        ])
      ),
      providersRead()
    );
    renderTasks('/', () => <TaskTree />);

    expect(await screen.findByText('Readable work')).toBeInTheDocument();
    expect(screen.getByText('1 task could not be read and is not shown.')).toBeInTheDocument();
  });

  it('does not say there are no tasks when every row is unreadable', async () => {
    server.use(
      tasksList(() => HttpResponse.json([{ task: { id: 'broken' } }, null])),
      providersRead()
    );
    renderTasks('/', () => <TaskTree />);

    expect(
      await screen.findByText('2 tasks could not be read and are not shown.')
    ).toBeInTheDocument();
    expect(screen.queryByText('No tasks yet.')).not.toBeInTheDocument();
  });
});
