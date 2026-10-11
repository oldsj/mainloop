import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { server } from '@/test/msw/server';
import { schedule } from './test/fixtures';
import { problem, schedulesRead, schedulesReadWith } from './test/handlers';
import { renderSchedules } from './test/render';

// The page's reads would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

const PAUSED = schedule({
  id: 'sch-2',
  config: { ...schedule().config, name: 'Weekly sweep', paused: true, schedule: '30 9 * * 1' }
});

describe('schedule list', () => {
  it('lists each schedule with its plain-words time, state and a link to its page', async () => {
    server.use(schedulesRead([{ items: [schedule(), PAUSED] }]));
    renderSchedules('/schedules');

    const nightly = await screen.findByRole('link', { name: /Nightly triage/ });
    expect(nightly).toHaveAttribute('href', '/schedules/sch-1');
    expect(within(nightly).getByText('Every day at 02:00')).toBeInTheDocument();
    expect(within(nightly).getByText('Active')).toBeInTheDocument();

    const weekly = screen.getByRole('link', { name: /Weekly sweep/ });
    expect(weekly).toHaveAttribute('href', '/schedules/sch-2');
    expect(within(weekly).getByText('Every Monday at 09:30')).toBeInTheDocument();
    expect(within(weekly).getByText('Paused', { selector: 'span' })).toBeInTheDocument();
  });

  it('offers a new schedule when there are none', async () => {
    server.use(schedulesRead([{ items: [] }]));
    renderSchedules('/schedules');

    expect(await screen.findByText('No schedules yet.')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'New schedule' })).toHaveAttribute(
      'href',
      '/schedules/new'
    );
  });

  it('loads the next page on request', async () => {
    server.use(schedulesRead([{ items: [schedule()] }, { items: [PAUSED] }]));
    renderSchedules('/schedules');

    await screen.findByRole('link', { name: /Nightly triage/ });
    expect(screen.queryByRole('link', { name: /Weekly sweep/ })).not.toBeInTheDocument();

    await userEvent.setup().click(screen.getByRole('button', { name: 'Load more' }));
    expect(await screen.findByRole('link', { name: /Weekly sweep/ })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Load more' })).not.toBeInTheDocument();
  });

  it('shows the failure with a retry that recovers', async () => {
    let reads = 0;
    server.use(
      schedulesReadWith((info) => {
        reads += 1;
        return reads === 1
          ? problem(500, 'Store unavailable')(info)
          : HttpResponse.json({ items: [schedule()], next_page_token: null });
      })
    );
    renderSchedules('/schedules');

    const alert = await screen.findByRole('alert');
    expect(within(alert).getByText('Could not load schedules')).toBeInTheDocument();
    expect(within(alert).getByText('Store unavailable')).toBeInTheDocument();

    await userEvent.setup().click(within(alert).getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('link', { name: /Nightly triage/ })).toBeInTheDocument();
  });
});
