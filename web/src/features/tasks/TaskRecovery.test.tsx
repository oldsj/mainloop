import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { QueryClient } from '@tanstack/react-query';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection, keys } from '@/api';
import { createSseApplier } from '@/api/invalidation';
import type { TaskView } from '@/lib/contract';
import { server } from '@/test/msw/server';
import { operation, reassignedView, view } from '@/test/fixtures/tasks';
import { renderTasks } from './test/render';
import {
  allowedView,
  providersRead,
  serveTaskPage,
  taskAction,
  taskRead,
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

/**
 * Ports of the frozen Svelte recovery tests (`frontend/src/lib/taskRecovery.test.ts`, tests 1 and 3).
 * The store and its SSE wiring are replaced by TanStack Query and `createSseApplier`; the owner-facing
 * behaviour each frozen test pins down is asserted here through the page.
 */

function deferred() {
  let release!: () => void;
  const promise = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}

/** The task after the lost request landed elsewhere: attempt 2 on Codex, reassign no longer offered. */
function movedView(): TaskView {
  const moved = reassignedView();
  return {
    ...moved,
    task: { ...moved.task, id: 'task-1' },
    attempts: moved.attempts.map((row) => ({ ...row, task_id: 'task-1' })),
    operations: moved.operations.map((row) => ({ ...row, task_id: 'task-1' })),
    actions: { ...moved.actions, reassign: { available: false, reason: 'handoff_unavailable' } }
  };
}

/** Waits until nothing is in flight, so a read the test triggered has settled before it asserts. */
async function settle(queryClient: QueryClient) {
  await waitFor(() => expect(queryClient.isFetching()).toBe(0));
}

describe('frozen recovery: lost reply stays resendable', () => {
  it('keeps a lost reassign resendable with its original version, attempt and target', async () => {
    const page = taskScreen(view());
    const bodies: unknown[] = [];
    let calls = 0;
    const release = deferred();
    server.use(
      ...serveTaskPage(page),
      taskAction('reassign', async ({ request }) => {
        bodies.push(await request.json());
        calls += 1;
        if (calls === 1) {
          // The server may have recorded the request; the reply never arrives. The task has moved on.
          page.view = movedView();
          return HttpResponse.error();
        }
        await release.promise;
        return HttpResponse.json(operation({ kind: 'reassign' }));
      })
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    await user.click(await screen.findByTestId('task-action-reassign'));

    const resend = await screen.findByTestId('task-resend');
    // The lost reply marked the connection offline, which disables resend. A successful probe
    // clears it, as the live probe loop does in the app, before the owner can resend.
    await act(() => connection.check());
    expect(
      within(resend).getByText(
        'Original reassign: version 3, attempt attempt-1, target codex-default.'
      )
    ).toBeInTheDocument();
    // The page re-read the task before it offered the resend, so the owner sees the current version.
    expect(screen.getByText(/Acting on version 9, attempt 2\./)).toBeInTheDocument();
    expect(screen.getByTestId('task-action-reassign')).toBeDisabled();
    expect(within(resend).getByRole('button', { name: 'Resend original request' })).toBeEnabled();

    const resendButton = within(resend).getByRole('button', { name: 'Resend original request' });
    await user.click(resendButton);
    expect(await screen.findByText('Sending request…')).toBeInTheDocument();
    expect(resendButton).toBeDisabled();
    await user.click(resendButton);
    expect(calls).toBe(2);

    release.release();
    expect(
      await screen.findByText('Reassign requested. Progress shows below as the server reports it.')
    ).toBeInTheDocument();
    expect(screen.queryByTestId('task-resend')).not.toBeInTheDocument();
    expect(bodies[0]).toEqual({
      request_id: expect.stringMatching(/^ui-/),
      expected_version: 3,
      expected_attempt_id: 'attempt-1',
      target_profile_id: 'codex-default'
    });
    // The resend is the same logical request: same ID, same version, attempt and target.
    expect(bodies[1]).toEqual(bodies[0]);
  });
});

describe('frozen recovery: background reads warn on the cached task', () => {
  it('warns on the cached task when a background read fails, and only that task success clears it', async () => {
    const page = taskScreen(allowedView(), [view({ id: 'other-task', title: 'Other work' })]);
    let taskDown = false;
    let listDown = false;
    server.use(
      taskRead(() => {
        if (taskDown) return HttpResponse.json({ detail: 'offline refresh' }, { status: 500 });
        return HttpResponse.json(page.view);
      }),
      tasksList(() =>
        listDown
          ? HttpResponse.json({ detail: 'list offline' }, { status: 500 })
          : HttpResponse.json(page.list)
      ),
      providersRead(),
      taskAction('retry', () => HttpResponse.json(operation({ kind: 'retry' })))
    );
    const user = userEvent.setup();
    const { queryClient } = renderTasks('/tasks/task-1');
    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });
    expect(screen.queryByText(/Showing the last known state/)).not.toBeInTheDocument();

    const apply = createSseApplier(queryClient);
    // The first connect needs nothing: the queries are already loading.
    apply({ event: 'connected', data: '' });

    // Trigger 1: a live update says the task moved on, and the read for it fails.
    taskDown = true;
    apply({
      event: 'task:updated',
      data: JSON.stringify({ event_id: 'background-1', task_id: 'task-1', version: 4 })
    });
    await settle(queryClient);
    expect(await screen.findByText(/Showing the last known state/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Retry loading' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 1, name: 'Add retry button' })).toBeInTheDocument();

    // Trigger 2: the stream reconnects, which refetches everything, and the read still fails.
    apply({ event: 'connected', data: '' });
    await settle(queryClient);
    expect(screen.getByText(/Showing the last known state/)).toBeInTheDocument();

    // Trigger 3: an action succeeds, and the re-read after it fails.
    await user.click(screen.getByTestId('task-action-retry'));
    expect(await screen.findByTestId('task-action-notice')).toHaveTextContent('Retry requested.');
    await settle(queryClient);
    expect(screen.getByText(/Showing the last known state/)).toBeInTheDocument();

    // A failed list is reported on its own and does not touch the task's warning.
    listDown = true;
    await act(() => queryClient.invalidateQueries({ queryKey: keys.tasks() }));
    expect(
      await screen.findByText('Parent and child tasks could not be loaded.')
    ).toBeInTheDocument();
    expect(screen.getByText(/Showing the last known state/)).toBeInTheDocument();

    // A list that recovers does not clear the task's warning either.
    listDown = false;
    await act(() => queryClient.invalidateQueries({ queryKey: keys.tasks() }));
    await waitFor(() =>
      expect(
        screen.queryByText('Parent and child tasks could not be loaded.')
      ).not.toBeInTheDocument()
    );
    expect(screen.getByText(/Showing the last known state/)).toBeInTheDocument();

    // Only the task's own read, succeeding, clears it.
    taskDown = false;
    await user.click(screen.getByRole('button', { name: 'Retry loading' }));
    await waitFor(() =>
      expect(screen.queryByText(/Showing the last known state/)).not.toBeInTheDocument()
    );
    expect(screen.queryByRole('button', { name: 'Retry loading' })).not.toBeInTheDocument();
  });
});
