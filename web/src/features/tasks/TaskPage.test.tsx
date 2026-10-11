import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { server } from '@/test/msw/server';
import { operation, projection, view } from '@/test/fixtures/tasks';
import { renderTasks } from './test/render';
import {
  problem,
  providersRead,
  serveTaskPage,
  taskAction,
  taskProblem,
  taskRead,
  allowedView,
  taskScreen,
  tasksList
} from './test/handlers';

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

const REQUEST_ID = expect.stringMatching(/^ui-/);

describe('task page layout', () => {
  it('shows the sections in the frozen order, each under its own heading', async () => {
    server.use(...serveTaskPage(taskScreen(view())));
    renderTasks('/tasks/task-1');

    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });
    const headings = screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent);
    expect(headings).toEqual([
      '[PROVIDER]',
      '[WORKSPACE]',
      '[PR · CI · MERGE]',
      '[IN PROGRESS]',
      '[ACTIONS]',
      '[ATTEMPTS]',
      '[REPORTS]',
      '[CHILD TASKS]'
    ]);
  });

  it('reports the provider, publication and attempts under their test ids', async () => {
    server.use(...serveTaskPage(taskScreen(view())));
    renderTasks('/tasks/task-1');

    expect(await screen.findByTestId('task-provider')).toHaveTextContent('claude');
    expect(screen.getByTestId('task-status')).toHaveTextContent(/running/i);
    expect(screen.getByTestId('task-publication')).toHaveTextContent('Publication:');
    expect(screen.getByTestId('task-operations')).toHaveTextContent('No operation in progress.');
    expect(screen.getByTestId('task-attempts')).toHaveTextContent('Attempt 1 · claude');
  });

  it('marks observed facts stale and says each is observed separately', async () => {
    const stale = view({}, { projection: projection({ observed_at: '2020-01-01T00:00:00Z' }) });
    server.use(...serveTaskPage(taskScreen(stale)));
    renderTasks('/tasks/task-1');

    const observed = await screen.findByTestId('task-observed');
    expect(observed).toHaveTextContent('(stale)');
    expect(observed).toHaveTextContent('Each is observed separately; none implies the others.');
  });

  it('shows the unverified provider summary under its label', async () => {
    const summarised = view(
      {},
      {
        artifacts: [
          {
            id: 'artifact-1',
            operation_id: 'operation-done',
            kind: 'unverified_provider_summary',
            sha256: 'a'.repeat(64),
            payload: { summary: 'I finished the button.' }
          }
        ]
      }
    );
    server.use(...serveTaskPage(taskScreen(summarised)));
    renderTasks('/tasks/task-1');

    const section = await screen.findByTestId('task-unverified-summary');
    expect(within(section).getByText('I finished the button.')).toBeInTheDocument();
    expect(within(section).getByText(/has not verified it/)).toBeInTheDocument();
  });

  it('lists the pending owner actions and links to the Inbox', async () => {
    const waiting = view(
      { reason: 'approval' },
      { projection: projection({ pending_approval_ids: ['approval-1'] }) }
    );
    server.use(...serveTaskPage(taskScreen(waiting)));
    renderTasks('/tasks/task-1');

    const panel = await screen.findByTestId('task-pending-approvals');
    expect(within(panel).getByText('1 pending owner action')).toBeInTheDocument();
    expect(within(panel).getByRole('link', { name: 'Open Inbox' })).toHaveAttribute(
      'href',
      '/?tab=inbox'
    );
  });

  it('says so when an approval reason has no pending request behind it', async () => {
    server.use(...serveTaskPage(taskScreen(view({ reason: 'approval' }))));
    renderTasks('/tasks/task-1');

    expect(
      await screen.findByText(/Waiting on owner approval, but the server lists no pending request/)
    ).toBeInTheDocument();
  });

  it('links a child to its parent and lists its children', async () => {
    const parent = view({ id: 'task-parent', title: 'Parent work' });
    const child = view({ id: 'task-child', title: 'Child work', parent_task_id: 'task-1' });
    server.use(
      ...serveTaskPage(taskScreen(view({ parent_task_id: 'task-parent' }), [parent, child]))
    );
    renderTasks('/tasks/task-1');

    expect(await screen.findByRole('link', { name: 'Parent work' })).toHaveAttribute(
      'href',
      '/tasks/task-parent'
    );
    expect(await screen.findByRole('link', { name: /Child work/ })).toHaveAttribute(
      'href',
      '/tasks/task-child'
    );
  });

  it('keeps every button a button and every target at least 44px', async () => {
    server.use(...serveTaskPage(taskScreen(view())));
    renderTasks('/tasks/task-1');
    await screen.findByTestId('task-action-retry');

    const buttons = screen.getAllByRole('button');
    expect(buttons.length).toBeGreaterThan(0);
    for (const button of buttons) {
      expect(button).toHaveAttribute('type', 'button');
      expect(button).toHaveClass('min-h-11');
    }
    for (const link of screen.getAllByRole('link')) {
      expect(link).toHaveClass('min-h-11');
    }
  });
});

describe('task page loading and failure', () => {
  it('reports a malformed payload without crashing', async () => {
    server.use(
      taskRead(() => HttpResponse.json({ task: { id: 'task-1' } })),
      tasksList(() => HttpResponse.json([])),
      providersRead()
    );
    renderTasks('/tasks/task-1');

    expect(
      await screen.findByText('Mainloop sent a task this page cannot read.')
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Retry loading' })).toBeInTheDocument();
  });

  it('keeps the task on screen when the parent and child list cannot load', async () => {
    server.use(
      taskRead(() => HttpResponse.json(view())),
      tasksList(problem(500, 'list offline')),
      providersRead()
    );
    renderTasks('/tasks/task-1');

    expect(
      await screen.findByText('Parent and child tasks could not be loaded.')
    ).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 1, name: 'Add retry button' })).toBeInTheDocument();
    expect(screen.queryByText(/Showing the last known state/)).not.toBeInTheDocument();
  });

  it('shows a not-found page for a task the server does not have', async () => {
    server.use(
      taskRead(problem(404, 'Not Found')),
      tasksList(() => HttpResponse.json([])),
      providersRead()
    );
    renderTasks('/tasks/task-missing');

    expect(await screen.findByTestId('task-not-found')).toHaveTextContent('Task not found');
  });

  it('says the backend is unreachable when no response arrives', async () => {
    server.use(
      taskRead(() => HttpResponse.error()),
      tasksList(() => HttpResponse.json([])),
      providersRead()
    );
    renderTasks('/tasks/task-1');

    expect(await screen.findByText("Can't reach the Mainloop backend.")).toBeInTheDocument();
  });
});

describe('owner actions', () => {
  it('leaves an action off with the server reason when it is not available', async () => {
    server.use(...serveTaskPage(taskScreen(view())));
    renderTasks('/tasks/task-1');

    expect(await screen.findByTestId('task-action-retry')).toBeDisabled();
    expect(screen.getByText('handoff is not available yet')).toBeInTheDocument();
  });

  it('asks twice before cancelling, and sends the version and attempt the owner saw', async () => {
    const bodies: unknown[] = [];
    server.use(
      ...serveTaskPage(taskScreen(view())),
      taskAction('cancel', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(operation({ kind: 'cancel' }));
      })
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    await user.click(await screen.findByTestId('task-action-cancel'));
    const dialog = await screen.findByRole('alertdialog');
    expect(within(dialog).getByText('Cancel this task?')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Keep running' }));
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument());
    expect(bodies).toEqual([]);

    await user.click(screen.getByTestId('task-action-cancel'));
    const again = await screen.findByRole('alertdialog');
    await user.click(within(again).getByRole('button', { name: 'Confirm cancel' }));

    expect(
      await screen.findByText('Cancel requested. Progress shows below as the server reports it.')
    ).toBeInTheDocument();
    expect(bodies).toEqual([
      { request_id: REQUEST_ID, expected_version: 3, expected_attempt_id: 'attempt-1' }
    ]);
  });

  it('reassigns to the other provider with the target the owner saw', async () => {
    const bodies: unknown[] = [];
    server.use(
      ...serveTaskPage(taskScreen(view())),
      taskAction('reassign', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(operation({ kind: 'reassign' }));
      })
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    const picker = await screen.findByLabelText('Provider to reassign to');
    expect(
      await within(picker).findByRole('option', { name: 'Codex (codex)' })
    ).toBeInTheDocument();
    await user.click(screen.getByTestId('task-action-reassign'));

    expect(
      await screen.findByText('Reassign requested. Progress shows below as the server reports it.')
    ).toBeInTheDocument();
    expect(bodies).toEqual([
      {
        request_id: REQUEST_ID,
        expected_version: 3,
        expected_attempt_id: 'attempt-1',
        target_profile_id: 'codex-default'
      }
    ]);
  });

  it('sends one request when the owner presses twice', async () => {
    const gate = deferred();
    let calls = 0;
    server.use(
      ...serveTaskPage(taskScreen(view())),
      taskAction('reassign', async () => {
        calls += 1;
        await gate.promise;
        return HttpResponse.json(operation({ kind: 'reassign' }));
      })
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    const reassign = await screen.findByTestId('task-action-reassign');
    await waitFor(() => expect(reassign).toBeEnabled());
    await user.click(reassign);
    await user.click(reassign);
    expect(reassign).toBeDisabled();
    expect(calls).toBe(1);

    gate.release();
    expect(
      await screen.findByText('Reassign requested. Progress shows below as the server reports it.')
    ).toBeInTheDocument();
    expect(calls).toBe(1);
  });

  it('clears the notice when the owner moves to another task', async () => {
    const other = view({ id: 'task-2', title: 'Other task' });
    server.use(
      taskRead(({ params }) =>
        HttpResponse.json(params.task_id === 'task-2' ? other : allowedView())
      ),
      tasksList(() => HttpResponse.json([])),
      providersRead(),
      taskAction('cancel', () => HttpResponse.json(operation({ kind: 'cancel' })))
    );
    const user = userEvent.setup();
    const { router } = renderTasks('/tasks/task-1');

    await user.click(await screen.findByTestId('task-action-cancel'));
    await user.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Confirm cancel' })
    );
    expect(
      await screen.findByText('Cancel requested. Progress shows below as the server reports it.')
    ).toBeInTheDocument();

    await act(() => router.navigate({ to: '/tasks/$id', params: { id: 'task-2' } }));

    expect(
      await screen.findByRole('heading', { level: 1, name: 'Other task' })
    ).toBeInTheDocument();
    expect(screen.queryByTestId('task-action-notice')).not.toBeInTheDocument();
  });

  it('re-reads the task on a 409 before it says the task changed', async () => {
    const page = taskScreen(view());
    server.use(
      ...serveTaskPage(page),
      taskAction('reassign', (info) => {
        page.view = view({ title: 'Renamed by someone else', version: 4 });
        return taskProblem(409, 'stale_task_version')(info);
      })
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    await user.click(await screen.findByTestId('task-action-reassign'));

    expect(
      await screen.findByText(
        'The task changed since you loaded it. Review the latest state and try again.'
      )
    ).toBeInTheDocument();
    expect(screen.getByTestId('task-action-notice')).toHaveTextContent(
      'Server reason: stale_task_version'
    );
    // The notice appears only after the re-read: the page already shows the new title.
    expect(page.reads).toBe(2);
    expect(
      screen.getByRole('heading', { level: 1, name: 'Renamed by someone else' })
    ).toBeInTheDocument();
  });

  it('shows the backend reason in words and quotes its code', async () => {
    server.use(
      ...serveTaskPage(taskScreen(view())),
      taskAction('reassign', taskProblem(409, 'parent_capacity'))
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    await user.click(await screen.findByTestId('task-action-reassign'));

    expect(
      await screen.findByText('The parent task has no capacity for another attempt right now.')
    ).toBeInTheDocument();
    expect(screen.getByTestId('task-action-notice')).toHaveTextContent(
      'Server reason: parent_capacity'
    );
  });

  // Last in the file: the connection store is a module singleton and stays offline afterwards.
  it('disables the actions and says so while the connection is down', async () => {
    server.use(...serveTaskPage(taskScreen(allowedView())));
    renderTasks('/tasks/task-1');
    expect(await screen.findByTestId('task-action-retry')).toBeEnabled();

    // Nothing is probed here: the store is only told the connection failed, and it is never restarted.
    act(() => connection.reportFailure());

    expect(await screen.findAllByText('Mainloop is offline.')).not.toHaveLength(0);
    expect(screen.getByTestId('task-action-retry')).toBeDisabled();
    expect(screen.getByTestId('task-action-cancel')).toBeDisabled();
  });
});
