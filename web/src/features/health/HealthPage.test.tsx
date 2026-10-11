import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { native } from '@/features/sessions/test/fixtures';
import { nativeRead, providersRead } from '@/features/sessions/test/handlers';
import { server } from '@/test/msw/server';
import { CLAUDE_PROFILE } from '@/test/fixtures/tasks';
import {
  approvalText,
  droppedObservationsText,
  droppedRowsText,
  EMPTY_NOTICE,
  gapText,
  MALFORMED_OBSERVATIONS,
  MALFORMED_VIEW,
  OBSERVATIONS_EMPTY,
  OBSERVATIONS_PARTIAL,
  PARTIAL_NOTICE,
  reportText,
  retentionText
} from './model';
import { events, healthRow, healthView, session, toolEvent, totals } from './test/fixtures';
import { eventsRead, healthRead, projectsRead } from './test/handlers';
import { renderHealth } from './test/render';

// The page's reads would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

vi.mock('@/features/hitl', () => ({
  HITLCard: ({ requestId }: { requestId: string }) => (
    <div data-testid="hitl-card">{requestId}</div>
  ),
  HITLRequest: () => null
}));

const DESKTOP_PX = 1024;

function setWidth(px: number) {
  Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: px });
}

// Each test replaces these responders to say what the server returns. The defaults are a good read.
let requests: URLSearchParams[];
let eventRequests: URLSearchParams[];
let respond: (query: URLSearchParams) => Response;
let respondEvents: (query: URLSearchParams) => Response;

beforeEach(() => {
  requests = [];
  eventRequests = [];
  respond = () => HttpResponse.json(healthView([healthRow()]));
  respondEvents = () =>
    HttpResponse.json(
      events(
        [
          toolEvent(),
          {
            event_id: 'event-bad',
            payload: { kind: 'weird', sequence: 4, source: 'claude_result_stream' }
          }
        ],
        true
      )
    );
  server.use(
    healthRead(({ request }) => {
      const query = new URL(request.url).searchParams;
      requests.push(query);
      return respond(query);
    }),
    eventsRead(({ request }) => {
      const query = new URL(request.url).searchParams;
      eventRequests.push(query);
      return respondEvents(query);
    }),
    projectsRead([]),
    providersRead([CLAUDE_PROFILE]),
    nativeRead(native({ session_id: 'session-1', kind: 'claude', model: 'claude-sonnet-5-5' }))
  );
});

afterEach(async () => {
  vi.restoreAllMocks();
  setWidth(DESKTOP_PX);
  await connection.check();
});

const ok = () => HttpResponse.json(healthView([healthRow()]));
const broken = () => HttpResponse.json({ detail: 'Store is down' }, { status: 500 });

/** The row summary button. Its accessible name starts with the row title, so it is not the Task option of the same name. */
const rowButton = (title: string) => screen.findByRole('button', { name: new RegExp(`^${title}`) });

/** Opens the default row, the one most tests use. */
async function openRow(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await rowButton('Add retry button'));
  return screen.findByRole('region', { name: 'Session Retry work' });
}

describe('health page: the request', () => {
  it('asks for the last 24 hours and sends no empty filters', async () => {
    renderHealth('/health');
    await rowButton('Add retry button');
    expect(requests.map((query) => query.toString())).toEqual(['hours=24']);
  });

  it('reads the filters from the URL', async () => {
    renderHealth('/health?task_id=task-1&hours=168');
    expect(
      await screen.findByRole('button', { name: 'Filters · last 7 days' })
    ).toBeInTheDocument();
    expect(
      requests.some((query) => query.get('task_id') === 'task-1' && query.get('hours') === '168')
    ).toBe(true);
  });

  it('drops an unreadable filter to its default instead of failing the page', async () => {
    renderHealth('/health?hours=abc&provider=gpt');
    await rowButton('Add retry button');
    expect(requests.map((query) => query.toString())).toEqual(['hours=24']);
    expect(screen.getByLabelText('Provider')).toHaveValue('');
  });

  it('writes Apply to the URL and asks again with the new window', async () => {
    const user = userEvent.setup();
    const { router } = renderHealth('/health');
    await rowButton('Add retry button');

    await user.selectOptions(screen.getByLabelText('Window'), '720');
    await user.click(screen.getByRole('button', { name: 'Apply filters' }));

    await waitFor(() => expect(requests.some((query) => query.get('hours') === '720')).toBe(true));
    expect(router.state.location.href).toContain('hours=720');
    expect(
      await screen.findByRole('button', { name: 'Filters · last 30 days' })
    ).toBeInTheDocument();
  });

  it('clears every filter back to the default window', async () => {
    const user = userEvent.setup();
    const { router } = renderHealth('/health?task_id=task-1&hours=168');
    await screen.findByRole('button', { name: 'Filters · last 7 days' });

    await user.click(screen.getByRole('button', { name: 'Clear filters' }));

    await waitFor(() => expect(router.state.location.href).not.toContain('task_id'));
    await waitFor(() => expect(requests.at(-1)?.toString()).toBe('hours=24'));
  });

  it('offers task choices from the wider scope when a task is selected', async () => {
    const other = healthRow({
      id: 'row-2',
      task_id: 'task-2',
      title: 'Other task',
      sessions: [session({ session_id: 'session-2', attempt_id: 'attempt-2', task_id: 'task-2' })]
    });
    // A narrowed request returns only the selected task; the wider one returns both.
    respond = (query) =>
      HttpResponse.json(healthView(query.has('task_id') ? [healthRow()] : [healthRow(), other]));

    renderHealth('/health?task_id=task-1');

    const tasks = await screen.findByLabelText('Task');
    await waitFor(() =>
      expect(within(tasks).getByRole('option', { name: 'Other task' })).toBeInTheDocument()
    );
    expect(requests.some((query) => !query.has('task_id'))).toBe(true);
  });
});

describe('health page: the filter panel', () => {
  it('starts closed below 640 px', async () => {
    setWidth(400);
    renderHealth('/health');
    await rowButton('Add retry button');
    expect(screen.getByRole('button', { name: /^Filters ·/ })).toHaveAttribute(
      'aria-expanded',
      'false'
    );
    expect(screen.queryByRole('form', { name: 'Health filters' })).not.toBeInTheDocument();
  });

  it('starts open at 640 px and up', async () => {
    setWidth(640);
    renderHealth('/health');
    await rowButton('Add retry button');
    expect(screen.getByRole('button', { name: /^Filters ·/ })).toHaveAttribute(
      'aria-expanded',
      'true'
    );
    expect(screen.getByRole('form', { name: 'Health filters' })).toBeInTheDocument();
  });

  it('closes when the width drops below 640 px, and the owner can open it again', async () => {
    const user = userEvent.setup();
    renderHealth('/health');
    await rowButton('Add retry button');
    expect(screen.getByRole('form', { name: 'Health filters' })).toBeInTheDocument();

    setWidth(400);
    act(() => {
      window.dispatchEvent(new Event('resize'));
    });
    await waitFor(() =>
      expect(screen.queryByRole('form', { name: 'Health filters' })).not.toBeInTheDocument()
    );

    await user.click(screen.getByRole('button', { name: /^Filters ·/ }));
    expect(screen.getByRole('form', { name: 'Health filters' })).toBeInTheDocument();
  });
});

describe('health page: rows', () => {
  it('sorts rows by failures, most first, with unknown counts last', async () => {
    const alpha = healthRow({
      id: 'a',
      task_id: 'task-a',
      title: 'Alpha',
      totals: totals({ failures: 1 })
    });
    const beta = healthRow({
      id: 'b',
      task_id: 'task-b',
      title: 'Beta',
      totals: totals({ failures: null })
    });
    const gamma = healthRow({
      id: 'c',
      task_id: 'task-c',
      title: 'Gamma',
      totals: totals({ failures: 5 })
    });
    respond = () => HttpResponse.json(healthView([alpha, beta, gamma]));

    renderHealth('/health');

    const items = await screen.findAllByRole('listitem');
    expect(items).toHaveLength(3);
    expect(items[0]).toHaveTextContent('Gamma');
    expect(items[1]).toHaveTextContent('Alpha');
    expect(items[2]).toHaveTextContent('Beta');
    expect(within(items[2]!).getByText('Unavailable')).toBeInTheDocument();
  });

  it('labels a row with no task or title as Unassigned', async () => {
    respond = () =>
      HttpResponse.json(
        healthView([healthRow({ id: 'orphan', task_id: null, title: null, sessions: [] })])
      );
    renderHealth('/health');
    expect(await screen.findByText('Unassigned')).toBeInTheDocument();
  });

  it('drops a malformed row, says so, and still shows the rest', async () => {
    respond = () =>
      HttpResponse.json(
        healthView([healthRow({ id: 'good' }), healthRow({ id: 'bad', totals: 'nope' })])
      );
    renderHealth('/health');

    expect(await screen.findByText(droppedRowsText(1))).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^Add retry button/ })).toBeInTheDocument();
    expect(screen.queryByText(EMPTY_NOTICE)).not.toBeInTheDocument();
  });

  it('shows the partial notice, the empty state and the retention footer', async () => {
    respond = () => HttpResponse.json(healthView([], { partial: true }));
    renderHealth('/health');

    expect(await screen.findByText(PARTIAL_NOTICE)).toBeInTheDocument();
    expect(screen.getByText(EMPTY_NOTICE)).toBeInTheDocument();
    expect(
      screen.getByText(retentionText({ events_days: 7, summaries_days: 90 }))
    ).toBeInTheDocument();
  });

  it('shows the task, gaps, claims, approvals and sessions when a row is expanded', async () => {
    const report = {
      id: 'report-1',
      task_id: 'task-1',
      attempt_id: 'attempt-1',
      outcome: 'blocked' as const
    };
    const approval = { id: 'approval-1', runtime_session_id: 'session-1', state: 'pending' };
    respond = () =>
      HttpResponse.json(
        healthView([
          healthRow({ gaps: ['tool_outcomes'], reports: [report], approvals: [approval] })
        ])
      );
    const user = userEvent.setup();
    renderHealth('/health');

    expect(screen.queryByText(gapText(['tool_outcomes']))).not.toBeInTheDocument();
    await openRow(user);

    expect(screen.getByRole('link', { name: 'Open task' })).toHaveAttribute(
      'href',
      '/tasks/task-1'
    );
    expect(screen.getByText(gapText(['tool_outcomes']))).toBeInTheDocument();
    expect(screen.getByText(reportText(report))).toBeInTheDocument();
    expect(screen.getByText(approvalText(approval))).toBeInTheDocument();
    expect(screen.getByTestId('hitl-card')).toHaveTextContent('approval-1');
    expect(screen.getByRole('link', { name: 'Retry work' })).toHaveAttribute(
      'href',
      '/sessions/session-1'
    );
    expect(await screen.findByText('claude · sonnet-5-5 · live')).toBeInTheDocument();
  });
});

describe('health page: source observations', () => {
  it("loads a session's observations on request, and keeps them shown when a reload fails", async () => {
    const user = userEvent.setup();
    let fail = false;
    respondEvents = () =>
      fail
        ? broken()
        : HttpResponse.json(
            events(
              [
                toolEvent(),
                {
                  event_id: 'event-bad',
                  payload: { kind: 'weird', sequence: 4, source: 'claude_result_stream' }
                }
              ],
              true
            )
          );
    renderHealth('/health');
    const section = await openRow(user);

    await user.click(within(section).getByRole('button', { name: 'Load source observations' }));
    expect(
      await within(section).findByText('shell · error · tool_error · src/app.ts')
    ).toBeInTheDocument();
    expect(eventRequests.map((query) => query.get('hours'))).toEqual(['24']);
    expect(screen.getByText(OBSERVATIONS_PARTIAL)).toBeInTheDocument();
    expect(screen.getByText(droppedObservationsText(1))).toBeInTheDocument();

    fail = true;
    await user.click(within(section).getByRole('button', { name: 'Load source observations' }));
    const alert = await within(section).findByRole('alert');
    expect(alert).toHaveTextContent("Couldn't refresh observations.");
    expect(
      within(section).getByText('shell · error · tool_error · src/app.ts')
    ).toBeInTheDocument();

    fail = false;
    await user.click(within(alert).getByRole('button', { name: 'Try again' }));
    await waitFor(() => expect(within(section).queryByRole('alert')).not.toBeInTheDocument());
    expect(eventRequests).toHaveLength(3);
  });

  it('shows an error with retry when observations cannot be loaded', async () => {
    const user = userEvent.setup();
    respondEvents = broken;
    renderHealth('/health');
    const section = await openRow(user);

    await user.click(within(section).getByRole('button', { name: 'Load source observations' }));
    const alert = await within(section).findByRole('alert');
    expect(alert).toHaveTextContent('Observations could not be loaded.');

    respondEvents = () => HttpResponse.json(events([toolEvent()]));
    await user.click(within(alert).getByRole('button', { name: 'Try again' }));
    expect(
      await within(section).findByText('shell · error · tool_error · src/app.ts')
    ).toBeInTheDocument();
  });

  it('says so when a session has no events in the window', async () => {
    const user = userEvent.setup();
    respondEvents = () => HttpResponse.json(events([]));
    renderHealth('/health');
    const section = await openRow(user);

    await user.click(within(section).getByRole('button', { name: 'Load source observations' }));
    expect(await within(section).findByText(OBSERVATIONS_EMPTY)).toBeInTheDocument();
  });

  it('reports observations that come back in a shape the page cannot read', async () => {
    const user = userEvent.setup();
    respondEvents = () => HttpResponse.json({ events: 'nope', partial: false });
    renderHealth('/health');
    const section = await openRow(user);

    await user.click(within(section).getByRole('button', { name: 'Load source observations' }));
    expect(await within(section).findByRole('alert')).toHaveTextContent(MALFORMED_OBSERVATIONS);
  });

  it('clears loaded observations when the page refreshes', async () => {
    const user = userEvent.setup();
    renderHealth('/health');
    const section = await openRow(user);

    await user.click(within(section).getByRole('button', { name: 'Load source observations' }));
    expect(
      await within(section).findByText('shell · error · tool_error · src/app.ts')
    ).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Refresh' }));
    await waitFor(() =>
      expect(
        within(section).queryByText('shell · error · tool_error · src/app.ts')
      ).not.toBeInTheDocument()
    );
    expect(
      within(section).getByRole('button', { name: 'Load source observations' })
    ).toBeInTheDocument();
  });
});

describe('health page: loading and failure', () => {
  it('shows an error with retry when health cannot be loaded', async () => {
    const user = userEvent.setup();
    respond = broken;
    renderHealth('/health');

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Health could not be loaded.');
    expect(alert).toHaveTextContent('Mainloop returned a server error.');

    respond = ok;
    await user.click(within(alert).getByRole('button', { name: 'Try again' }));
    expect(await rowButton('Add retry button')).toBeInTheDocument();
  });

  it('keeps the rows on a failed refresh, with an inline error and retry', async () => {
    const user = userEvent.setup();
    renderHealth('/health');
    await rowButton('Add retry button');

    respond = broken;
    await user.click(screen.getByRole('button', { name: 'Refresh' }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("Couldn't refresh health.");
    expect(alert).toHaveTextContent('The rows below are from the last successful read.');
    expect(screen.getByRole('button', { name: /^Add retry button/ })).toBeInTheDocument();

    respond = ok;
    await user.click(within(alert).getByRole('button', { name: 'Try again' }));
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
  });

  it('shows an error when the health response has a shape the page cannot read', async () => {
    respond = () => HttpResponse.json({ ...healthView([]), rows: 'nope' });
    renderHealth('/health');
    expect(await screen.findByRole('alert')).toHaveTextContent(MALFORMED_VIEW);
  });
});
