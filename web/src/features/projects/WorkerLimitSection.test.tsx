import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { server } from '@/test/msw/server';
import { workerLimit } from './test/fixtures';
import { problem, workerLimitRead, workerLimitWrite } from './test/handlers';
import { WorkerLimitSection, WorkersRunning } from './WorkerLimitSection';

// The section makes its reads through the app's limiter, which the tests skip. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

function renderSection() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  render(
    <QueryClientProvider client={queryClient}>
      <WorkersRunning projectId="project-a" />
      <WorkerLimitSection projectId="project-a" />
    </QueryClientProvider>
  );
  return queryClient;
}

const section = () => within(screen.getByTestId('worker-limit'));
const field = () => section().getByRole('spinbutton', { name: 'Workers at once' });

describe('the project’s worker limit', () => {
  it('says how many workers run, where the limit comes from, and that the owner-wide one applies', async () => {
    // Given a project on the default limit with two workers running
    server.use(workerLimitRead(() => HttpResponse.json(workerLimit())));
    renderSection();

    // Then the line beside the tasks and the section both say so
    expect(await screen.findByTestId('project-workers-running')).toHaveTextContent(
      '2 of 6 workers running'
    );
    expect(section().getByTestId('worker-limit-running')).toHaveTextContent(
      '2 of 6 workers running.'
    );
    expect(section().getByTestId('worker-limit-source')).toHaveTextContent(
      'This project uses the default limit of 6. The limit across all projects (20) applies as well. It is shared, so workers in other projects can hold this one back sooner.'
    );
    // And the field starts from the limit in force, with nothing to save or reset yet
    expect(field()).toHaveValue(6);
    expect(section().getByRole('button', { name: 'Save limit' })).toBeDisabled();
    expect(section().queryByRole('button', { name: /Use the default/ })).toBeNull();
  });

  it('says so when the owner-wide limit is the one that binds', async () => {
    // Given a project that asks for 30 under an owner-wide 20: the server caps what is in force
    server.use(
      workerLimitRead(() =>
        HttpResponse.json(
          workerLimit({ max_workers: 30, effective_max_workers: 20, workers_running: 20 })
        )
      )
    );
    renderSection();

    expect(await section().findByTestId('worker-limit-running')).toHaveTextContent(
      '20 of 20 workers running.'
    );
    expect(section().getByTestId('worker-limit-source')).toHaveTextContent(
      'This project’s own limit is 30. The default is 6. The limit across all projects is 20, which is lower, so at most 20 run here.'
    );
  });

  it('sets the project’s own limit, and shows the answer without a second read', async () => {
    const user = userEvent.setup();
    const bodies: unknown[] = [];
    let reads = 0;
    server.use(
      workerLimitRead(() => {
        reads += 1;
        return HttpResponse.json(workerLimit());
      }),
      workerLimitWrite(async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(workerLimit({ max_workers: 10, effective_max_workers: 10 }));
      })
    );
    renderSection();

    // When the owner types 10 and saves
    await user.clear(await screen.findByRole('spinbutton', { name: 'Workers at once' }));
    await user.type(field(), '10');
    await user.click(section().getByRole('button', { name: 'Save limit' }));

    // Then the new limit is in force everywhere it is shown, from the write's own answer
    await waitFor(() => expect(bodies).toEqual([{ max_workers: 10 }]));
    expect(await screen.findByTestId('project-workers-running')).toHaveTextContent(
      '2 of 10 workers running'
    );
    expect(section().getByTestId('worker-limit-source')).toHaveTextContent(
      'This project’s own limit is 10. The default is 6.'
    );
    expect(reads).toBe(1);
    // And the way back to the default is offered
    expect(section().getByRole('button', { name: 'Use the default (6)' })).toBeEnabled();
  });

  it('returns to the default by sending no limit of its own', async () => {
    const user = userEvent.setup();
    const bodies: unknown[] = [];
    server.use(
      workerLimitRead(() =>
        HttpResponse.json(workerLimit({ max_workers: 10, effective_max_workers: 10 }))
      ),
      workerLimitWrite(async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(workerLimit());
      })
    );
    renderSection();

    await user.click(await section().findByRole('button', { name: 'Use the default (6)' }));

    await waitFor(() => expect(bodies).toEqual([{ max_workers: null }]));
    await waitFor(() => expect(field()).toHaveValue(6));
    expect(section().queryByRole('button', { name: /Use the default/ })).toBeNull();
  });

  it.each(['0', '101', '2.5', ''])(
    'does not offer to save %j: only a whole number from 1 to 100',
    async (typed) => {
      const user = userEvent.setup();
      server.use(workerLimitRead(() => HttpResponse.json(workerLimit())));
      renderSection();

      await user.clear(await screen.findByRole('spinbutton', { name: 'Workers at once' }));
      if (typed) await user.type(field(), typed);

      expect(section().getByRole('button', { name: 'Save limit' })).toBeDisabled();
      expect(field()).toHaveAttribute('aria-invalid', 'true');
      expect(section().getByText('Enter a whole number from 1 to 100.')).toBeInTheDocument();
    }
  );

  it('keeps the old limit on screen and says why when the server refuses the change', async () => {
    const user = userEvent.setup();
    server.use(
      workerLimitRead(() => HttpResponse.json(workerLimit())),
      workerLimitWrite(problem(422, 'max_workers must be between 1 and 100'))
    );
    renderSection();

    await user.clear(await screen.findByRole('spinbutton', { name: 'Workers at once' }));
    await user.type(field(), '9');
    await user.click(section().getByRole('button', { name: 'Save limit' }));

    expect(await section().findByRole('alert')).toHaveTextContent('The limit was not changed.');
    expect(section().getByTestId('worker-limit-running')).toHaveTextContent(
      '2 of 6 workers running.'
    );
    // What the owner typed is still there to correct.
    expect(field()).toHaveValue(9);
  });

  it('says it could not load, with a way to try again, and leaves the line by the tasks out', async () => {
    const user = userEvent.setup();
    let fail = true;
    server.use(
      workerLimitRead(() =>
        fail
          ? HttpResponse.json({ detail: 'boom' }, { status: 500 })
          : HttpResponse.json(workerLimit())
      )
    );
    renderSection();

    expect(await section().findByText('Could not load the worker limit.')).toBeInTheDocument();
    expect(screen.queryByTestId('project-workers-running')).toBeNull();

    fail = false;
    await user.click(section().getByRole('button', { name: 'Try again' }));
    expect(await screen.findByTestId('project-workers-running')).toHaveTextContent(
      '2 of 6 workers running'
    );
  });
});
