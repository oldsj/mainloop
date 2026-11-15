import { act, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { API_URL } from '@/api/config';
import type { TaskFollowUpResult, TaskView } from '@/lib/contract';
import { server } from '@/test/msw/server';
import { view } from '@/test/fixtures/tasks';
import { renderTasks } from './test/render';
import { serveTaskPage, taskProblem, taskScreen } from './test/handlers';

afterEach(async () => {
  // A lost reply marks the shared connection offline, which would leak into the next test.
  await connection.check();
});

// Reads skip the app's rate limiter here; see fastReads.ts for why this mock sits in each test file.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('./test/fastReads');
  return withFastReads(await importOriginal());
});

const followUp = (resolve: HttpResponseResolver) =>
  http.post(`${API_URL}/tasks/:task_id/follow-up`, resolve);

function result(overrides: Partial<TaskFollowUpResult> = {}): TaskFollowUpResult {
  return {
    task_id: 'task-1',
    attempt_id: 'attempt-1',
    session_id: 'session-1',
    message_id: 'message-1',
    delivery: 'sent',
    reopened: false,
    version: 4,
    ...overrides
  };
}

function followable(available = true): TaskView {
  const base = view({ status: 'completed', reason: null });
  return {
    ...base,
    actions: {
      ...base.actions,
      follow_up: available
        ? { available: true, reason: null }
        : { available: false, reason: 'follow_up_unavailable' }
    }
  };
}

describe('follow up', () => {
  it('sends the message to the task and says the task reopened', async () => {
    const bodies: unknown[] = [];
    server.use(
      ...serveTaskPage(taskScreen(followable())),
      followUp(async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(result({ reopened: true }), { status: 202 });
      })
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    const box = await screen.findByTestId('task-follow-up');
    const send = within(box).getByRole('button', { name: 'Send' });
    expect(send).toBeDisabled();
    await user.type(within(box).getByRole('textbox', { name: 'Follow up' }), 'and staging?');
    await user.click(send);

    expect(
      await screen.findByText('Sent to the task. The task reopened on its session.')
    ).toBeInTheDocument();
    expect(bodies).toEqual([{ request_id: expect.stringMatching(/^ui-/), text: 'and staging?' }]);
    expect(within(box).getByRole('textbox', { name: 'Follow up' })).toHaveValue('');
  });

  it('is not offered when the server does not allow it', async () => {
    server.use(...serveTaskPage(taskScreen(followable(false))));
    renderTasks('/tasks/task-1');
    await screen.findByTestId('task-actions');
    expect(screen.queryByTestId('task-follow-up')).not.toBeInTheDocument();
  });

  it('resends a lost message with its first request ID', async () => {
    const bodies: { request_id: string }[] = [];
    server.use(
      ...serveTaskPage(taskScreen(followable())),
      followUp(async ({ request }) => {
        bodies.push((await request.json()) as { request_id: string });
        if (bodies.length === 1) return HttpResponse.error();
        return HttpResponse.json(result({ delivery: 'queued' }), { status: 202 });
      })
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    const box = await screen.findByTestId('task-follow-up');
    await user.type(within(box).getByRole('textbox', { name: 'Follow up' }), 'rebase it');
    await user.click(within(box).getByRole('button', { name: 'Send' }));
    expect(await screen.findByText(/did not confirm the message/)).toBeInTheDocument();
    // The text is kept for the resend.
    expect(within(box).getByRole('textbox', { name: 'Follow up' })).toHaveValue('rebase it');

    await act(() => connection.check());
    await user.click(within(box).getByRole('button', { name: 'Send' }));
    expect(await screen.findByText('Queued behind the turn that is running.')).toBeInTheDocument();
    expect(bodies).toHaveLength(2);
    expect(bodies[1]!.request_id).toBe(bodies[0]!.request_id);
  });

  it('says in words why the server refused', async () => {
    server.use(
      ...serveTaskPage(taskScreen(followable())),
      followUp(taskProblem(409, 'session_gone'))
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    const box = await screen.findByTestId('task-follow-up');
    await user.type(within(box).getByRole('textbox', { name: 'Follow up' }), 'one more');
    await user.click(within(box).getByRole('button', { name: 'Send' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("This task's native session is gone");
    expect(alert).toHaveTextContent('session_gone');
  });
});
