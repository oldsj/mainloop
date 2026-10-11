import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { server } from '@/test/msw/server';
import { execution, schedule } from './test/fixtures';
import {
  executionsRead,
  problem,
  scheduleDelete,
  scheduleReadWith,
  scheduleTrigger,
  scheduleUpdate,
  schedulesRead
} from './test/handlers';
import { renderSchedules } from './test/render';
import type { ScheduleConfigInput, ScheduleView } from './model';

// The page's reads would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

type Write = { ifMatch: string | null; body: ScheduleConfigInput };

/**
 * Serves one schedule, its executions and its writes. `outcomes` decides what the next writes do:
 * "changed" simulates another saver (412), "deleted" a removed schedule (409); anything else saves.
 */
function serve(
  initial: ScheduleView = schedule(),
  outcomes: ('changed' | 'deleted' | 'ok')[] = []
) {
  const store = { view: initial, writes: [] as Write[], outcomes: [...outcomes] };
  server.use(
    scheduleReadWith(() => HttpResponse.json(store.view)),
    executionsRead([execution()]),
    scheduleUpdate(async ({ request }) => {
      const body = (await request.json()) as ScheduleConfigInput;
      store.writes.push({ ifMatch: request.headers.get('If-Match'), body });
      const outcome = store.outcomes.shift() ?? 'ok';
      if (outcome === 'changed') {
        store.view = schedule({
          etag: 'etag-2',
          config: { ...store.view.config, name: 'Changed by someone else' }
        });
        return HttpResponse.json({ detail: 'Schedule changed' }, { status: 412 });
      }
      if (outcome === 'deleted') {
        return HttpResponse.json({ detail: 'Schedule was deleted' }, { status: 409 });
      }
      store.view = schedule({
        etag: `etag-${store.writes.length + 1}`,
        config: { ...store.view.config, ...body }
      });
      return HttpResponse.json(store.view);
    })
  );
  return store;
}

describe('schedule page', () => {
  it('shows a not-found message for a schedule that is gone', async () => {
    server.use(scheduleReadWith(problem(404, 'Schedule not found')), executionsRead([]));
    renderSchedules('/schedules/sch-1');

    expect(await screen.findByText('This schedule no longer exists.')).toBeInTheDocument();
  });

  it('saves edits with the etag it read, and the next save uses the newer one', async () => {
    const store = serve();
    const user = userEvent.setup();
    renderSchedules('/schedules/sch-1');
    const name = await screen.findByLabelText('Name');
    await user.type(name, ' (edited)');
    await user.click(screen.getByRole('button', { name: 'Save changes' }));

    expect(await screen.findByRole('status')).toHaveTextContent('Saved.');
    expect(store.writes[0]?.ifMatch).toBe('"etag-1"');
    expect(store.writes[0]?.body).toMatchObject({ name: 'Nightly triage (edited)' });
    // Waits for the re-read after the save, so the next write carries the newer etag.
    await screen.findByRole('heading', { level: 2, name: /edited/ });

    await user.click(screen.getByRole('button', { name: 'Pause' }));
    await waitFor(() => expect(store.writes).toHaveLength(2));
    expect(store.writes[1]?.ifMatch).toBe('"etag-2"');
  });

  it('keeps the user edits when someone else saved first, and shows the newer values beside them', async () => {
    const store = serve(schedule(), ['changed']);
    const user = userEvent.setup();
    renderSchedules('/schedules/sch-1');
    const name = await screen.findByLabelText('Name');
    await user.clear(name);
    await user.type(name, 'My edit');
    await user.click(screen.getByRole('button', { name: 'Save changes' }));

    const notice = await screen.findByRole('alert');
    expect(within(notice).getByText('Changed elsewhere')).toBeInTheDocument();
    // Waits for the re-read that the conflict triggers, so the next save carries the newer etag.
    expect(
      await within(notice).findByText('Changed by someone else', { selector: 'dd' })
    ).toBeInTheDocument();
    expect(screen.getByLabelText('Name')).toHaveValue('My edit');

    await user.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(store.writes).toHaveLength(2));
    expect(store.writes[1]).toMatchObject({ ifMatch: '"etag-2"', body: { name: 'My edit' } });
    await waitFor(() => expect(screen.queryByText('Changed elsewhere')).not.toBeInTheDocument());
  });

  it('can discard the edits and take the saved version after a conflict', async () => {
    serve(schedule(), ['changed']);
    const user = userEvent.setup();
    renderSchedules('/schedules/sch-1');
    const name = await screen.findByLabelText('Name');
    await user.clear(name);
    await user.type(name, 'My edit');
    await user.click(screen.getByRole('button', { name: 'Save changes' }));

    const notice = await screen.findByRole('alert');
    await within(notice).findByText('Changed by someone else', { selector: 'dd' });
    await user.click(
      within(notice).getByRole('button', { name: 'Discard my edits and use the saved version' })
    );

    expect(screen.getByLabelText('Name')).toHaveValue('Changed by someone else');
    expect(screen.queryByText('Changed elsewhere')).not.toBeInTheDocument();
  });

  it('locks the form when the schedule was deleted elsewhere', async () => {
    serve(schedule(), ['deleted']);
    const user = userEvent.setup();
    renderSchedules('/schedules/sch-1');
    const name = await screen.findByLabelText('Name');
    await user.type(name, ' edit');
    await user.click(screen.getByRole('button', { name: 'Save changes' }));

    expect(await screen.findByText('Deleted elsewhere')).toBeInTheDocument();
    expect(screen.getByLabelText('Name')).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Save changes' })).toBeDisabled();
  });

  it('pauses with the saved configuration, not the unsaved form edits', async () => {
    const store = serve();
    const user = userEvent.setup();
    renderSchedules('/schedules/sch-1');
    await user.type(await screen.findByLabelText('Name'), ' unsaved');
    await user.click(screen.getByRole('button', { name: 'Pause' }));

    expect(await screen.findByRole('status')).toHaveTextContent('Paused. No runs will start.');
    expect(store.writes[0]).toMatchObject({
      ifMatch: '"etag-1"',
      body: { paused: true, name: 'Nightly triage', schedule: '0 2 * * *' }
    });
    expect(screen.getByRole('button', { name: 'Resume' })).toBeInTheDocument();
    expect(screen.getByLabelText('Name')).toHaveValue('Nightly triage unsaved');
  });

  it('starts a run now with a request id and says where it appears', async () => {
    const triggers: unknown[] = [];
    serve();
    server.use(
      scheduleTrigger(async ({ request }) => {
        triggers.push(await request.json());
        return HttpResponse.json(execution({ state: 'pending' }), { status: 202 });
      })
    );
    const user = userEvent.setup();
    renderSchedules('/schedules/sch-1');
    await user.click(await screen.findByRole('button', { name: 'Run now' }));

    expect(await screen.findByRole('status')).toHaveTextContent('Run requested.');
    expect(triggers).toEqual([{ request_id: expect.any(String) }]);
  });

  it('confirms before deleting, and a cancel sends nothing', async () => {
    const deletes: string[] = [];
    serve();
    server.use(
      scheduleDelete(({ params }) => {
        deletes.push(String(params.schedule_id));
        return new HttpResponse(null, { status: 204 });
      }),
      schedulesRead([{ items: [] }])
    );
    const user = userEvent.setup();
    renderSchedules('/schedules/sch-1');
    await user.click(await screen.findByRole('button', { name: 'Delete' }));

    let dialog = await screen.findByRole('alertdialog');
    expect(within(dialog).getByText('Delete this schedule?')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument());
    expect(deletes).toHaveLength(0);

    await user.click(screen.getByRole('button', { name: 'Delete' }));
    dialog = await screen.findByRole('alertdialog');
    await user.click(within(dialog).getByRole('button', { name: 'Delete' }));

    expect(await screen.findByText('No schedules yet.')).toBeInTheDocument();
    expect(deletes).toEqual(['sch-1']);
  });

  it('lists executions and links each one that has a session to it', async () => {
    serve();
    server.use(
      executionsRead([
        execution({ id: 'exe-1', session_id: 'sess-1', state: 'succeeded' }),
        execution({
          id: 'exe-2',
          scheduled_time: null,
          session_id: null,
          state: 'failed',
          failure_reason: 'Agent timed out'
        })
      ])
    );
    const user = userEvent.setup();
    renderSchedules('/schedules/sch-1');

    const links = await screen.findAllByRole('link', { name: 'Open session' });
    expect(links).toHaveLength(1);
    expect(links[0]).toHaveAttribute('href', '/sessions/sess-1');
    expect(screen.getByText('Manual run')).toBeInTheDocument();
    expect(screen.getByText('No session yet.')).toBeInTheDocument();
    expect(screen.getByText('Agent timed out')).toBeInTheDocument();

    await user.click(links[0]!);
    expect(await screen.findByText('Session page')).toBeInTheDocument();
  });
});
