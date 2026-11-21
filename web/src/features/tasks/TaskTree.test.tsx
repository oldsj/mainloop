import { act, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection, keys } from '@/api';
import { reasonLabel } from '@/lib/taskState';
import { projection, view } from '@/test/fixtures/tasks';
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
      it(`says nothing about CI for ${ci_state} with ${label}`, async () => {
        server.use(
          tasksList(() =>
            HttpResponse.json([
              view({}, { projection: projection({ ci_state, ...evidenceNow(evidence) }) })
            ])
          ),
          providersRead()
        );
        renderTasks('/', () => <TaskTree />);

        // CI that is not known is left out: the row never says "CI unknown", and never guesses.
        expect(await screen.findByTestId('task-state')).not.toHaveTextContent(/CI/);
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

      expect(await screen.findByTestId('task-state')).toHaveTextContent(
        new RegExp(`· CI ${EXPECTED_WORD[ci_state]}$`)
      );
    });
  }
});

describe('task tree', () => {
  it('nests a child under its parent, one step in, with a guide line for the level above', async () => {
    const parent = view({ id: 'task-parent', title: 'Parent work' });
    const child = view({ id: 'task-child', title: 'Child work', parent_task_id: 'task-parent' });
    server.use(
      tasksList(() => HttpResponse.json([parent, child])),
      providersRead()
    );
    renderTasks('/', () => <TaskTree />);

    const row = await screen.findByRole('link', { name: /Child work/ });
    const parentRow = screen.getByRole('link', { name: /Parent work/ });
    expect(row.closest('li')).toHaveAttribute('data-depth', '1');
    expect(parentRow.closest('li')).toHaveAttribute('data-depth', '0');
    expect(within(row).getAllByTestId('task-indent-guide')).toHaveLength(1);
    expect(within(parentRow).queryByTestId('task-indent-guide')).toBeNull();
    // The child follows its parent directly.
    const rows = screen.getAllByRole('link').map((link) => link.getAttribute('href'));
    expect(rows).toEqual(['/tasks/task-parent', '/tasks/task-child']);
    expect(row).toHaveClass('min-h-control');
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
    expect(within(row).getByTestId('task-state')).toHaveTextContent(
      /child of a task not shown here$/
    );
    expect(row.closest('li')).toHaveAttribute('data-depth', '0');
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
    // A plain word, not a bracketed tag.
    const state = within(row).getByTestId('task-state');
    expect(state).toHaveTextContent(/^supervisor · /);
    expect(state).toHaveTextContent(new RegExp(reasonLabel('owner_decision') as string));
    expect(row.textContent).not.toMatch(/\[/);
    // It waits on the owner, so its dot is the Needs-you red.
    expect(within(row).getByTestId('task-dot')).toHaveAttribute('data-tone', 'attention');
  });

  it('draws a cancelled supervisor task at rest', async () => {
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
    expect(within(row).getByTestId('task-state')).toHaveTextContent('supervisor · Cancelled');
    expect(within(row).getByTestId('task-dot')).toHaveAttribute('data-tone', 'muted');
  });

  it('folds cancelled tasks away behind one button, and never a cancelled task with open work under it', async () => {
    // Given a project with live work, two cancelled tasks, and a cancelled supervisor whose child still runs.
    const tasks = [
      view({
        id: 'live',
        title: 'Live work',
        status: 'running',
        created_at: '2026-01-05T00:00:00Z'
      }),
      view({ id: 'old-1', title: 'Dropped one', status: 'cancelled' }),
      view({ id: 'old-2', title: 'Dropped two', status: 'cancelled' }),
      view({ id: 'sup', title: 'Stopped supervisor', status: 'cancelled' }),
      view({ id: 'kid', title: 'Still running child', status: 'running', parent_task_id: 'sup' }),
      view({ id: 'done', title: 'Shipped', status: 'completed' })
    ];
    server.use(
      tasksList(() => HttpResponse.json(tasks)),
      providersRead()
    );

    // When the list opens.
    renderTasks('/', () => <TaskTree />);
    await screen.findByRole('link', { name: /Live work/ });

    // Then the two settled cancelled tasks are behind the button, and nothing with open work is.
    expect(screen.queryByRole('link', { name: /Dropped one/ })).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Stopped supervisor/ })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Still running child/ })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Shipped/ })).toBeInTheDocument();
    const toggle = screen.getByRole('button', { name: 'Show 2 cancelled tasks' });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');

    // And pressing it lists them after the rest, and offers to hide them again.
    await userEvent.click(toggle);
    const titles = screen.getAllByRole('link').map((link) => link.textContent ?? '');
    expect(titles.findIndex((t) => t.includes('Dropped one'))).toBeGreaterThan(
      titles.findIndex((t) => t.includes('Shipped'))
    );
    expect(screen.getByRole('button', { name: 'Hide cancelled tasks' })).toHaveAttribute(
      'aria-expanded',
      'true'
    );
  });

  it('lists a project of only cancelled tasks as it is, with no button', async () => {
    server.use(
      tasksList(() =>
        HttpResponse.json([view({ id: 'only', title: 'Only one', status: 'cancelled' })])
      ),
      providersRead()
    );
    renderTasks('/', () => <TaskTree />);

    expect(await screen.findByRole('link', { name: /Only one/ })).toBeInTheDocument();
    expect(screen.queryByTestId('task-list-cancelled-toggle')).not.toBeInTheDocument();
  });

  it('is one line: the dot, the title, the state and the age', async () => {
    const changed = new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString();
    server.use(
      tasksList(() => HttpResponse.json([view({ title: 'Ship it', updated_at: changed })])),
      providersRead()
    );
    renderTasks('/', () => <TaskTree />);

    const row = await screen.findByRole('link', { name: /Ship it/ });
    expect(within(row).getByTestId('task-dot')).toHaveAttribute('data-tone', 'active');
    expect(within(row).getByText('Ship it')).toHaveClass('truncate');
    expect(within(row).getByTestId('task-state')).toHaveTextContent('Running');
    expect(within(row).getByTestId('task-age')).toHaveTextContent('3h');
    // A narrow screen hides the longer state and keeps the status word, so it is never colour only.
    expect(within(row).getByTestId('task-status')).toHaveTextContent('Running');
    expect(within(row).getByTestId('task-status')).toHaveClass('sm:hidden');
  });

  it('says nothing about CI on a finished task, even when CI is known', async () => {
    const done = view(
      { title: 'Done work', status: 'completed' },
      {
        projection: projection({
          ci_state: 'success',
          pr_head_sha: 'aaa',
          ci_head_sha: 'aaa',
          observed_at: new Date().toISOString()
        })
      }
    );
    server.use(
      tasksList(() => HttpResponse.json([done])),
      providersRead()
    );
    renderTasks('/', () => <TaskTree />);

    const row = await screen.findByRole('link', { name: /Done work/ });
    expect(within(row).getByTestId('task-state')).toHaveTextContent(/^Completed$/);
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
