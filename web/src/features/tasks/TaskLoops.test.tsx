import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { API_URL } from '@/api/config';
import type { TaskLoop } from '@/lib/contract';
import { firing, loop, view } from '@/test/fixtures/tasks';
import { server } from '@/test/msw/server';
import { serveTaskPage, taskScreen } from './test/handlers';
import { renderTasks } from './test/render';

afterEach(async () => {
  // A lost reply marks the shared connection offline, which would leak into the next test.
  await connection.check();
});

// Reads skip the app's rate limiter here; see fastReads.ts for why this mock sits in each test file.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('./test/fastReads');
  return withFastReads(await importOriginal());
});

const url = (path: string) => `${API_URL}${path}`;

/** Serves the task page with these loops, mutable between reads. */
function serveLoops(loops: TaskLoop[]) {
  const state = { loops, reads: 0 };
  server.use(
    http.get(url('/tasks/:task_id/loops'), () => {
      state.reads += 1;
      return HttpResponse.json({ loops: state.loops });
    }),
    ...serveTaskPage(taskScreen(view()))
  );
  return state;
}

async function loopItem() {
  const section = await screen.findByTestId('task-loops');
  return within(section).getByTestId('task-loop');
}

describe('task page loops', () => {
  it('leaves the section out while the task has no loops', async () => {
    serveLoops([]);
    renderTasks('/tasks/task-1');
    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });
    await waitFor(() => expect(screen.queryByTestId('task-loops')).not.toBeInTheDocument());
  });

  it('shows what a loop waits for, whom it wakes, its firings and its latest messages', async () => {
    serveLoops([
      loop(),
      loop({
        id: 'loop-2',
        state: 'exhausted',
        trigger: 'quiet',
        every_minutes: null,
        quiet_minutes: 30,
        expect_change: true,
        stop_reason: 'no_progress',
        task_id: 'task-2',
        recipient_task_id: 'task-0',
        firings: 3,
        next_fire_at: null,
        recent_firings: [firing({ number: 3, stops_loop: 'no_progress', folded: 2 })]
      })
    ]);
    renderTasks('/tasks/task-1');

    const section = await screen.findByTestId('task-loops');
    const [timed, quiet] = within(section).getAllByTestId('task-loop');
    if (!timed || !quiet) throw new Error('expected two loops');
    expect(timed).toHaveTextContent('Every 30 minutes');
    expect(timed).toHaveTextContent('Armed');
    expect(timed).toHaveTextContent('wakes this task');
    expect(timed).toHaveTextContent('1 of 20 firings');
    expect(timed).toHaveTextContent(/next /);
    expect(timed).toHaveTextContent('Check the deploy.');
    expect(within(timed).getByText('30 minutes passed.')).toBeInTheDocument();
    for (const name of ['Pause', 'Fire now', 'Delete']) {
      expect(within(timed).getByRole('button', { name })).toBeInTheDocument();
    }

    expect(quiet).toHaveTextContent('When a watched task is quiet for 30 minutes');
    expect(quiet).toHaveTextContent('Stopped: no progress');
    expect(quiet).toHaveTextContent('wakes its supervisor');
    expect(quiet).toHaveTextContent('expects change');
    expect(quiet).toHaveTextContent('asked you to decide');
    expect(quiet).toHaveTextContent('2 more folded in');
    // An ended loop has no controls.
    expect(within(quiet).queryByRole('button')).not.toBeInTheDocument();
  });

  it('pauses, then resumes, showing the state the server returns', async () => {
    const state = serveLoops([loop()]);
    const calls: string[] = [];
    server.use(
      http.post(url('/loops/:loop_id/pause'), ({ params }) => {
        calls.push(`pause ${String(params.loop_id)}`);
        state.loops = [loop({ state: 'paused', next_fire_at: null })];
        return HttpResponse.json(state.loops[0]);
      }),
      http.post(url('/loops/:loop_id/resume'), ({ params }) => {
        calls.push(`resume ${String(params.loop_id)}`);
        state.loops = [loop()];
        return HttpResponse.json(state.loops[0]);
      })
    );
    renderTasks('/tasks/task-1');
    const user = userEvent.setup();

    await user.click(within(await loopItem()).getByRole('button', { name: 'Pause' }));
    const resume = await within(await loopItem()).findByRole('button', { name: 'Resume' });
    expect(await loopItem()).toHaveTextContent('Paused');
    await user.click(resume);
    await within(await loopItem()).findByRole('button', { name: 'Pause' });
    expect(calls).toEqual(['pause loop-1', 'resume loop-1']);
  });

  it('resends a fire whose reply was lost with the same request id', async () => {
    serveLoops([loop()]);
    const ids: string[] = [];
    server.use(
      http.post(url('/loops/:loop_id/fire'), async ({ request }) => {
        const body = (await request.json()) as { request_id: string };
        ids.push(body.request_id);
        if (ids.length === 1) return HttpResponse.error();
        return HttpResponse.json(loop({ firings: 2 }));
      })
    );
    renderTasks('/tasks/task-1');
    const user = userEvent.setup();

    await user.click(within(await loopItem()).getByRole('button', { name: 'Fire now' }));
    expect(await within(await loopItem()).findByRole('alert')).toHaveTextContent(
      'it will not fire twice'
    );
    await connection.check();
    await user.click(within(await loopItem()).getByRole('button', { name: 'Fire now' }));
    await waitFor(() => expect(ids).toHaveLength(2));
    expect(ids[0]).toMatch(/^ui-/);
    expect(ids[1]).toBe(ids[0]);
    await waitFor(() =>
      expect(within(screen.getByTestId('task-loops')).queryByRole('alert')).toBeNull()
    );
  });

  it('deletes only after a second click, and says when the loop changed first', async () => {
    const state = serveLoops([loop()]);
    let deletes = 0;
    server.use(
      http.delete(url('/loops/:loop_id'), () => {
        deletes += 1;
        state.loops = [loop({ state: 'stopped', next_fire_at: null })];
        return HttpResponse.json({ detail: { reason: 'loop_state_conflict' } }, { status: 409 });
      })
    );
    renderTasks('/tasks/task-1');
    const user = userEvent.setup();

    await user.click(within(await loopItem()).getByRole('button', { name: 'Delete' }));
    expect(deletes).toBe(0);
    await user.click(within(await loopItem()).getByRole('button', { name: 'Delete loop' }));
    expect(await within(await loopItem()).findByRole('alert')).toHaveTextContent(
      'This loop changed before the request landed'
    );
    expect(deletes).toBe(1);
    // The refusal re-reads the loops, which now show it stopped and without controls.
    await waitFor(async () => expect(await loopItem()).toHaveTextContent('Stopped'));
    expect(within(await loopItem()).queryByRole('button', { name: 'Fire now' })).toBeNull();
  });
});
