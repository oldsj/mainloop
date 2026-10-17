import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { profile } from '@/test/fixtures/tasks';
import { server } from '@/test/msw/server';
import { schedule } from './test/fixtures';
import {
  executionsRead,
  problem,
  providersRead,
  scheduleCreate,
  scheduleRead
} from './test/handlers';
import { renderSchedules } from './test/render';
import type { ScheduleConfigInput } from './model';

// The page's reads would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

// The first render of this file can be slow under a full run (see `fillForm`'s own wait below);
// the whole test needs more than the default budget to still have time for the rest of its steps.
vi.setConfig({ testTimeout: 15000 });

const CLAUDE = profile({
  id: 'claude-default',
  display_name: 'Claude',
  native_provider: 'claude',
  agents: { worker: { namespace: 'agents', name: 'triage' } }
});

type Post = { request_id: string; agent: unknown; config: ScheduleConfigInput };

async function fillForm(user: ReturnType<typeof userEvent.setup>) {
  // The first render of a test file can be slow under a full run, so this wait is longer than the default.
  await screen.findByRole('option', { name: 'Claude, worker: triage' }, { timeout: 5000 });
  await user.selectOptions(screen.getByLabelText('Agent'), 'agents/triage');
  await user.type(screen.getByLabelText('Name'), 'Nightly triage');
  await user.clear(screen.getByLabelText('Schedule (five fields)'));
  await user.type(screen.getByLabelText('Schedule (five fields)'), '0 2 * * *');
  await user.type(screen.getByLabelText('Prompt'), 'Triage the inbox');
}

describe('create schedule', () => {
  it('posts the agent and config with a request id, then opens the new schedule', async () => {
    const posts: Post[] = [];
    server.use(
      providersRead([CLAUDE]),
      scheduleCreate(async ({ request }) => {
        posts.push((await request.json()) as Post);
        return HttpResponse.json(schedule(), {
          status: 201,
          headers: { ETag: '"etag-1"' }
        });
      }),
      scheduleRead(schedule()),
      executionsRead([])
    );
    const user = userEvent.setup();
    const { router } = renderSchedules('/schedules/new');
    await fillForm(user);
    await user.click(screen.getByRole('button', { name: 'Create schedule' }));

    expect(
      await screen.findByRole('heading', { level: 2, name: /Nightly triage/ })
    ).toBeInTheDocument();
    expect(posts).toHaveLength(1);
    expect(posts[0]).toMatchObject({
      request_id: expect.any(String),
      agent: { namespace: 'agents', name: 'triage' },
      config: {
        name: 'Nightly triage',
        schedule: '0 2 * * *',
        prompt: 'Triage the inbox',
        paused: false,
        execution_timeout_seconds: 900
      }
    });
    expect(router.state.location.pathname).toBe('/schedules/sch-1');
  });

  it('starts the time zone at the browser zone, and sends UTC when it is cleared', async () => {
    const posts: Post[] = [];
    server.use(
      providersRead([CLAUDE]),
      scheduleCreate(async ({ request }) => {
        posts.push((await request.json()) as Post);
        return HttpResponse.json(schedule(), { status: 201 });
      }),
      scheduleRead(schedule()),
      executionsRead([])
    );
    const user = userEvent.setup();
    renderSchedules('/schedules/new');
    await fillForm(user);
    const zone = screen.getByLabelText('Time zone');
    expect(zone).toHaveValue(Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC');
    await user.clear(zone);
    await user.click(screen.getByRole('button', { name: 'Create schedule' }));

    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0]?.config.time_zone).toBe('UTC');
  });

  it('keeps the request id across a failed attempt, so a retry cannot create a second schedule', async () => {
    const posts: Post[] = [];
    let attempts = 0;
    server.use(
      providersRead([CLAUDE]),
      scheduleCreate(async ({ request }) => {
        posts.push((await request.json()) as Post);
        attempts += 1;
        if (attempts === 1)
          return HttpResponse.json({ detail: 'Store unavailable' }, { status: 500 });
        return HttpResponse.json(schedule(), { status: 201 });
      }),
      scheduleRead(schedule()),
      executionsRead([])
    );
    const user = userEvent.setup();
    renderSchedules('/schedules/new');
    await fillForm(user);
    await user.click(screen.getByRole('button', { name: 'Create schedule' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('Not created. Store unavailable');
    await user.click(screen.getByRole('button', { name: 'Create schedule' }));
    await screen.findByRole('heading', { level: 2, name: /Nightly triage/ });

    expect(posts).toHaveLength(2);
    expect(posts[1]?.request_id).toBe(posts[0]?.request_id);
  });

  it('shows an agent the deployment does not own as a plain message', async () => {
    server.use(
      providersRead([CLAUDE]),
      scheduleCreate(problem(403, 'Agent is not owned by this deployment.'))
    );
    const user = userEvent.setup();
    renderSchedules('/schedules/new');
    await fillForm(user);
    await user.click(screen.getByRole('button', { name: 'Create schedule' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Not created. Agent is not owned by this deployment.'
    );
  });

  it('asks for an agent before it sends anything', async () => {
    const posts: Post[] = [];
    server.use(
      providersRead([CLAUDE]),
      scheduleCreate(async ({ request }) => {
        posts.push((await request.json()) as Post);
        return HttpResponse.json(schedule(), { status: 201 });
      })
    );
    const user = userEvent.setup();
    renderSchedules('/schedules/new');
    // The first render of a test file can be slow under a full run, so this wait is longer than the default.
    await screen.findByRole('option', { name: 'Claude, worker: triage' }, { timeout: 5000 });
    await user.type(screen.getByLabelText('Prompt'), 'Triage the inbox');
    await user.click(screen.getByRole('button', { name: 'Create schedule' }));

    expect(await screen.findByText('Choose the agent this schedule runs on.')).toBeInTheDocument();
    expect(screen.getByLabelText('Agent')).toHaveAttribute('aria-invalid', 'true');
    expect(posts).toHaveLength(0);
  });

  it('shows the cron reading as the expression changes, and flags an invalid one', async () => {
    server.use(providersRead([CLAUDE]));
    const user = userEvent.setup();
    renderSchedules('/schedules/new');
    // The first render of a test file can be slow under a full run, so this wait is longer than the default.
    const cron = await screen.findByLabelText('Schedule (five fields)', {}, { timeout: 5000 });

    await user.clear(cron);
    await user.type(cron, '*/15 * * * *');
    expect(screen.getByText('Every 15 minutes')).toBeInTheDocument();
    expect(cron).not.toHaveAttribute('aria-invalid');

    await user.clear(cron);
    await user.type(cron, '99 * * * *');
    expect(cron).toHaveAttribute('aria-invalid', 'true');
    expect(screen.queryByText('Every 15 minutes')).not.toBeInTheDocument();
  });
});
