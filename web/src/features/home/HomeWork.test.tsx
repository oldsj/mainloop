import { screen, within } from '@testing-library/react';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import { keys } from '@/api';
import { server } from '@/test/msw/server';
import { projection, view } from '@/test/fixtures/tasks';
import { tasksList } from '@/features/tasks/test/handlers';
import { writeLastVisit } from './lastVisit';
import { projectsList } from './test/handlers';
import { renderHome } from './test/render';

const MIN = 60_000;
const minutesAgo = (minutes: number) => new Date(Date.now() - minutes * MIN).toISOString();
const PROJECTS = [{ id: 'project-1', owner: 'oldsj', name: 'mainloop' }];

afterEach(() => window.localStorage.clear());

function serve(
  rows: ReturnType<typeof view>[],
  projects: { id: string; owner: string; name: string }[] = PROJECTS
) {
  server.use(
    tasksList(() => HttpResponse.json(rows)),
    projectsList(projects)
  );
}

describe('Work list', () => {
  it('groups tasks and links each row to its task', async () => {
    serve([
      view({
        id: 'run-1',
        title: 'Fix sweep pinning',
        status: 'running',
        updated_at: minutesAgo(3)
      }),
      view({
        id: 'ask-1',
        title: 'Pick a branch',
        status: 'waiting',
        reason: 'owner_decision',
        updated_at: minutesAgo(1)
      }),
      view({ id: 'fail-1', title: 'Broken migration', status: 'failed', updated_at: minutesAgo(9) })
    ]);
    renderHome();

    const needs = await screen.findByRole('region', { name: 'Needs you (1)' });
    expect(within(needs).getByRole('link', { name: /Pick a branch/ })).toHaveAttribute(
      'href',
      '/tasks/ask-1'
    );
    const working = screen.getByRole('region', { name: 'Working (1)' });
    expect(within(working).getByRole('link', { name: /Fix sweep pinning/ })).toHaveAttribute(
      'href',
      '/tasks/run-1'
    );
    expect(screen.getByRole('region', { name: 'Blocked or failed (1)' })).toBeInTheDocument();
  });

  it('names each row with its project and a one-line status with its relative time', async () => {
    serve([
      view({
        id: 'run-1',
        title: 'Fix sweep pinning',
        status: 'waiting',
        reason: 'ci',
        project_id: 'project-1',
        updated_at: minutesAgo(12)
      })
    ]);
    renderHome();
    const row = await screen.findByTestId('work-row');
    expect(row).toHaveTextContent('oldsj/mainloop');
    expect(row).toHaveTextContent('Waiting · awaiting CI');
    expect(row).toHaveTextContent('12 min ago');
  });

  it('counts a running task as current: it appears under Working, never as nothing running', async () => {
    serve([view({ id: 'run-1', title: 'Still going', status: 'running' })]);
    renderHome();
    expect(await screen.findByText('Still going')).toBeInTheDocument();
    expect(screen.queryByText(/Nothing running/)).toBeNull();
  });

  it('shows the Working empty state with a pointer to main chat when nothing runs', async () => {
    serve([]);
    renderHome();
    expect(
      await screen.findByText('Nothing running. Start work from the main chat.')
    ).toBeInTheDocument();
    expect(screen.queryByText(/Nothing needs you/)).toBeNull();
    expect(screen.queryByRole('region', { name: /Needs you/ })).toBeNull();
  });

  it('hides a finished task that is older than the last visit', async () => {
    writeLastVisit(Date.now() - 60 * MIN);
    serve([
      view({ id: 'old', title: 'Old merge', status: 'completed', updated_at: minutesAgo(600) })
    ]);
    renderHome();
    await screen.findByText('Nothing running. Start work from the main chat.');
    expect(screen.queryByText('Old merge')).toBeNull();
  });

  it('shows a finished task from after the last visit under its own heading', async () => {
    writeLastVisit(Date.now() - 60 * MIN);
    serve([
      view({ id: 'new', title: 'Fresh merge', status: 'completed', updated_at: minutesAgo(5) })
    ]);
    renderHome();
    expect(
      await screen.findByRole('region', { name: 'Finished since you last looked (1)' })
    ).toHaveTextContent('Fresh merge');
  });

  it('says the list could not load instead of showing an empty list', async () => {
    server.use(tasksList(() => HttpResponse.json({ detail: 'boom' }, { status: 500 })));
    server.use(projectsList(PROJECTS));
    renderHome();
    expect(await screen.findByRole('alert')).toHaveTextContent('Couldn’t load tasks.');
  });

  it('keeps the held rows when a background read fails', async () => {
    serve([view({ id: 'run-1', title: 'Still going', status: 'running' })]);
    const { queryClient } = renderHome();
    expect(await screen.findByText('Still going')).toBeInTheDocument();
    server.use(tasksList(() => HttpResponse.json({ detail: 'boom' }, { status: 500 })));
    await queryClient.invalidateQueries({ queryKey: keys.tasks() });
    expect(screen.getByText('Still going')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('gives the rows and project names classes that stay inside a 390 px column', async () => {
    serve([
      view({
        id: 'run-1',
        title: 'A very long task title that must truncate rather than widen the column',
        status: 'running',
        project_id: 'project-1'
      })
    ]);
    renderHome();
    const row = await screen.findByTestId('work-row');
    expect(row.className).toContain('min-w-0');
    expect(row.querySelector('.truncate')).not.toBeNull();
    expect(screen.getByTestId('work-list').className).not.toMatch(
      /(^|\s)w-\d|(^|\s)min-w-\[|width:/
    );
  });
});

describe('Since you were here', () => {
  it('shows counts of what changed since the last visit, each linking to the latest task', async () => {
    writeLastVisit(Date.now() - 60 * MIN);
    serve([
      view(
        { id: 'merged-1', title: 'Shipped', status: 'completed', updated_at: minutesAgo(4) },
        {
          projection: projection({ merge_state: 'merged', pr_state: 'merged' })
        }
      ),
      view({ id: 'failed-1', title: 'Broke', status: 'failed', updated_at: minutesAgo(7) }),
      view({ id: 'failed-2', title: 'Broke again', status: 'failed', updated_at: minutesAgo(8) }),
      view({
        id: 'ask-1',
        title: 'Asks',
        status: 'waiting',
        reason: 'approval',
        updated_at: minutesAgo(2)
      })
    ]);
    renderHome();

    const strip = await screen.findByTestId('since-strip');
    expect(within(strip).getByTestId('since-waiting')).toHaveTextContent('1 task waiting on you');
    expect(within(strip).getByTestId('since-waiting')).toHaveAttribute('href', '/tasks/ask-1');
    expect(within(strip).getByTestId('since-failed')).toHaveTextContent('2 tasks failed');
    expect(within(strip).getByTestId('since-failed')).toHaveAttribute('href', '/tasks/failed-1');
    expect(within(strip).getByTestId('since-merged')).toHaveTextContent('1 task merged');
    expect(within(strip).getByTestId('since-merged')).toHaveAttribute('href', '/tasks/merged-1');
  });

  it('hides the strip when nothing changed since the last visit', async () => {
    writeLastVisit(Date.now() - 60 * MIN);
    serve([
      view({ id: 'old', title: 'Old failure', status: 'failed', updated_at: minutesAgo(900) })
    ]);
    renderHome();
    await screen.findByText('Old failure');
    expect(screen.queryByTestId('since-strip')).toBeNull();
  });

  it('hides the strip on a first visit, when there is no baseline', async () => {
    serve([view({ id: 'x', title: 'Broke once', status: 'failed', updated_at: minutesAgo(1) })]);
    renderHome();
    await screen.findByText('Broke once');
    expect(screen.queryByTestId('since-strip')).toBeNull();
  });

  it('records this visit so the next one compares against it', async () => {
    serve([]);
    renderHome();
    await screen.findByText('Nothing running. Start work from the main chat.');
    const recorded = Number(window.localStorage.getItem('mainloop.home.lastVisit'));
    expect(Math.abs(recorded - Date.now())).toBeLessThan(10_000);
  });
});
