import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it } from 'vitest';
import { API_URL } from '@/api/config';
import { keys } from '@/api';
import { hitlView } from '@/test/fixtures/hitl';
import { projection, view as taskView } from '@/test/fixtures/tasks';
import { server } from '@/test/msw/server';
import { createFakeHITL } from './handlers';
import { HITLCard } from './hitl-card';
import { renderRouted } from './test-utils';

let n = 0;
let id: string;
let fake: ReturnType<typeof createFakeHITL>;

beforeEach(() => {
  id = `card-${++n}`;
  fake = createFakeHITL({
    [id]: hitlView(id, { context: { project_id: 'p1', session_id: 's1', title: 'Fix login' } })
  });
  server.use(...fake.handlers);
});

describe('HITLCard', () => {
  it('loads the request, mirrors it into the query cache and links the owning task', async () => {
    server.use(
      http.get(`${API_URL}/tasks`, () =>
        HttpResponse.json([
          taskView(
            { id: 'task-1', title: 'Add retry button' },
            { projection: projection({ pending_approval_ids: [id] }) }
          )
        ])
      )
    );
    const { queryClient } = renderRouted(<HITLCard requestId={id} />);
    expect(screen.getByText('Loading session input…')).toBeInTheDocument();
    expect(await screen.findByRole('heading', { name: 'Session needs input' })).toBeInTheDocument();
    const link = await screen.findByTestId('hitl-task-link');
    expect(link).toHaveTextContent('Add retry button');
    expect(screen.getByRole('link', { name: 'Add retry button' })).toHaveAttribute(
      'href',
      '/tasks/task-1'
    );
    expect(queryClient.getQueryData(keys.hitl(id))).toMatchObject({ request: { id } });
  });

  it('shows no task link when no task is waiting on this request', async () => {
    server.use(http.get(`${API_URL}/tasks`, () => HttpResponse.json([])));
    renderRouted(<HITLCard requestId={id} />);
    await screen.findByRole('heading', { name: 'Session needs input' });
    await waitFor(() => expect(screen.queryByTestId('hitl-task-link')).not.toBeInTheDocument());
  });

  it('records one decision end to end and shows the receipt', async () => {
    server.use(http.get(`${API_URL}/tasks`, () => HttpResponse.json([])));
    const user = userEvent.setup();
    renderRouted(<HITLCard requestId={id} />);
    await screen.findByRole('heading', { name: 'Session needs input' });
    await user.click(await screen.findByRole('button', { name: /^Approve tool\.a/ }));
    await user.click(screen.getByRole('button', { name: /^Reject tool\.b/ }));
    await user.type(screen.getByLabelText(/Rejection reason/), 'Keep tests');
    const send = screen.getByRole('button', { name: 'Send response' });
    await user.click(send);
    await user.click(send);
    expect(await screen.findByText('Your recorded response')).toBeInTheDocument();
    expect(fake.calls).toHaveLength(1);
    expect(fake.calls[0]?.response).toMatchObject({
      approvals: [
        { id: 'a', approved: true },
        { id: 'b', approved: false, rejection_reason: 'Keep tests' }
      ]
    });
    expect(screen.getByRole('status')).toHaveTextContent('Decision recorded');
  });

  it('two cards for one request share one submit', async () => {
    server.use(http.get(`${API_URL}/tasks`, () => HttpResponse.json([])));
    const user = userEvent.setup();
    renderRouted(
      <>
        <HITLCard requestId={id} />
        <HITLCard requestId={id} />
      </>
    );
    await screen.findAllByRole('heading', { name: 'Session needs input' });
    const [first] = await screen.findAllByRole('button', { name: /^Approve tool\.a/ });
    await user.click(first!);
    await user.click(screen.getAllByRole('button', { name: /^Reject tool\.b/ })[0]!);
    await user.click(screen.getAllByRole('button', { name: 'Send response' })[0]!);
    await waitFor(() => expect(screen.getAllByText('Your recorded response')).toHaveLength(2));
    expect(fake.calls).toHaveLength(1);
  });

  it('a 404 on the first read offers a refresh instead of hanging', async () => {
    server.use(http.get(`${API_URL}/tasks`, () => HttpResponse.json([])));
    renderRouted(<HITLCard requestId="missing" />);
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not refresh this request');
    expect(screen.getByRole('button', { name: 'Refresh status' })).toBeEnabled();
  });

  it('shows the owner-write gate as a disabled state', async () => {
    server.use(http.get(`${API_URL}/tasks`, () => HttpResponse.json([])));
    fake.views.set(id, hitlView(id, { writes_enabled: false }));
    renderRouted(<HITLCard requestId={id} />);
    expect(await screen.findByText('Responding disabled by the server.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Send response' })).toBeDisabled();
  });
});
