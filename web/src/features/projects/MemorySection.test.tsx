import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection, keys } from '@/api';
import { server } from '@/test/msw/server';
import { memory, memoryEntry, type MemoryFixture } from './test/fixtures';
import { memoryDecide, memoryRead, problem, serveProjectPage } from './test/handlers';
import { renderProjects } from './test/render';

// The section's reads would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

const PAGE = '/projects/project-a';

const MAIN = memoryEntry();
const PROPOSAL = memoryEntry({
  id: 'mem-2',
  text: 'Deploys need a canary.',
  state: 'proposed',
  author: 'task',
  source: { session_id: 'sup-1', task_id: 'task-7', attempt_id: 'att-1', delivery_id: null },
  replaces_id: 'mem-1',
  decided_at: null
});

function section() {
  return screen.findByTestId('project-memory');
}

describe('memory section', () => {
  it('says when there is no memory', async () => {
    server.use(...serveProjectPage());
    renderProjects(PAGE);
    const memorySection = await section();
    expect(await within(memorySection).findByText('No memory yet.')).toBeInTheDocument();
    expect(within(memorySection).getByText('0 of 4,096 bytes used')).toBeInTheDocument();
    expect(within(memorySection).queryByTestId('memory-proposals')).not.toBeInTheDocument();
  });

  it('lists proposals for the owner to decide, then active entries, each with where it came from', async () => {
    server.use(
      ...serveProjectPage({
        memory: memory({ entries: [MAIN, PROPOSAL], used_bytes: MAIN.text.length })
      })
    );
    renderProjects(PAGE);
    const proposals = await screen.findByTestId('memory-proposals');
    expect(
      within(proposals).getByRole('heading', { name: 'Waiting for you (1)' })
    ).toBeInTheDocument();
    const proposal = within(proposals).getByTestId('memory-entry-mem-2');
    expect(within(proposal).getByText('Deploys need a canary.')).toBeInTheDocument();
    expect(within(proposal).getByText(`Replaces: ${MAIN.text}`)).toBeInTheDocument();
    expect(within(proposal).getByText(/^From a task, /)).toBeInTheDocument();
    expect(within(proposal).getByRole('link', { name: 'the task' })).toHaveAttribute(
      'href',
      '/tasks/task-7'
    );
    expect(
      within(proposal).getByRole('button', { name: 'Accept: Deploys need a canary.' })
    ).toBeEnabled();
    expect(
      within(proposal).getByRole('button', { name: 'Reject: Deploys need a canary.' })
    ).toBeEnabled();

    const active = screen.getByTestId('memory-active');
    const entry = within(active).getByTestId('memory-entry-mem-1');
    expect(within(entry).getByText(/^By main, /)).toBeInTheDocument();
    expect(within(entry).getByRole('link', { name: 'main thread' })).toHaveAttribute('href', '/');
    expect(within(entry).getByRole('button', { name: `Revoke: ${MAIN.text}` })).toBeInTheDocument();
    expect(within(active).getByText(`${MAIN.text.length} of 4,096 bytes used`)).toBeInTheDocument();
  });

  it('accepts a proposal, then shows the memory and Needs you as the server has them', async () => {
    let current: MemoryFixture = memory({
      entries: [MAIN, PROPOSAL],
      used_bytes: MAIN.text.length
    });
    let body: unknown;
    let decidedPath = '';
    server.use(
      // Ahead of the page's defaults, so its reads of the memory see what the decision left.
      memoryRead(() => HttpResponse.json(current)),
      memoryDecide(async ({ request, params }) => {
        body = await request.json();
        decidedPath = `${String(params.project_id)}/${String(params.entry_id)}`;
        const accepted = {
          ...PROPOSAL,
          state: 'active' as const,
          decided_at: '2026-11-02T12:00:00Z'
        };
        current = memory({ entries: [accepted], used_bytes: PROPOSAL.text.length });
        return HttpResponse.json(accepted);
      }),
      ...serveProjectPage()
    );
    const user = userEvent.setup();
    const { queryClient } = renderProjects(PAGE);
    // The Needs you snapshot the shell would hold, with the project's review item in it.
    queryClient.setQueryData(keys.attention(), { items: [], count: 1, sessions: [] });
    await user.click(await screen.findByRole('button', { name: 'Accept: Deploys need a canary.' }));

    await waitFor(() => expect(screen.queryByTestId('memory-proposals')).not.toBeInTheDocument());
    expect(body).toEqual({ decision: 'accept' });
    expect(decidedPath).toBe('project-a/mem-2');
    const active = screen.getByTestId('memory-active');
    expect(within(active).getByText(/^From a task, .*, accepted /)).toBeInTheDocument();
    expect(within(active).queryByTestId('memory-entry-mem-1')).not.toBeInTheDocument();
    expect(queryClient.getQueryState(keys.attention())?.isInvalidated).toBe(true);
  });

  it('shows a refused decision and the memory as it now is', async () => {
    server.use(
      ...serveProjectPage({
        memory: memory({ entries: [MAIN, PROPOSAL], used_bytes: MAIN.text.length })
      }),
      memoryDecide(problem(409, 'Project memory would pass 4096 bytes; revoke an entry first'))
    );
    const user = userEvent.setup();
    renderProjects(PAGE);
    await user.click(await screen.findByRole('button', { name: 'Accept: Deploys need a canary.' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Project memory would pass 4096 bytes; revoke an entry first'
    );
    expect(screen.getByTestId('memory-entry-mem-2')).toBeInTheDocument();
  });

  it('drops an entry that breaks the contract and keeps the rest', async () => {
    const broken = { ...memoryEntry({ id: 'mem-x', text: 'Broken' }), author: 'someone' };
    server.use(
      ...serveProjectPage({
        memory: memory({ entries: [MAIN, broken as never], used_bytes: MAIN.text.length })
      })
    );
    renderProjects(PAGE);
    expect(await screen.findByTestId('memory-entry-mem-1')).toBeInTheDocument();
    expect(screen.queryByText('Broken')).not.toBeInTheDocument();
  });

  it('shows a load failure with a retry', async () => {
    server.use(memoryRead(problem(500, 'boom')), ...serveProjectPage());
    renderProjects(PAGE);
    expect(await screen.findByText('Could not load the memory.')).toBeInTheDocument();
  });
});
