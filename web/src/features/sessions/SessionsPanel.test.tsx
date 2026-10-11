import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import { connection } from '@/api';
import { server } from '@/test/msw/server';
import { attentionSnapshot, session } from './test/fixtures';
import { attentionRead, otherListReads, problem, serveList, serveSession } from './test/handlers';
import { renderSessions } from './test/render';

afterEach(async () => {
  await connection.check();
});

describe('sessions list', () => {
  it('shows the empty state when nothing is current', async () => {
    server.use(...serveList([]));
    renderSessions('/');
    expect(await screen.findByText('No active sessions')).toBeInTheDocument();
  });

  it('lists current sessions with their titles and the current count', async () => {
    server.use(
      ...serveList([
        session({ id: 's1', title: 'Fix login redirect' }),
        session({ id: 's2', title: 'Write docs', status: 'pending' }),
        session({ id: 's3', title: 'Old run', status: 'completed' })
      ])
    );
    renderSessions('/');
    expect(await screen.findByText('Fix login redirect')).toBeInTheDocument();
    expect(screen.getByText('Write docs')).toBeInTheDocument();
    expect(screen.getByText('2 current')).toBeInTheDocument();
    expect(screen.getByTestId('show-finished')).toHaveTextContent('Show finished (1)');
  });

  it('labels each status, and shows a failed session with its error', async () => {
    server.use(
      ...serveList([
        session({ id: 's1', title: 'Queued', status: 'pending' }),
        session({ id: 's2', title: 'Running', status: 'active' }),
        session({
          id: 's3',
          title: 'Waiting',
          status: 'waiting_on_user',
          needs_owner: true
        }),
        session({ id: 's4', title: 'Finished', status: 'completed' }),
        session({ id: 's5', title: 'Broken', status: 'failed', error: 'boom' })
      ])
    );
    const user = userEvent.setup();
    renderSessions('/');
    await screen.findByText('Queued');
    await user.click(screen.getByTestId('show-finished'));

    expect(screen.getByText('PENDING')).toBeInTheDocument();
    expect(screen.getByText('ACTIVE')).toBeInTheDocument();
    expect(screen.getByText('NEEDS INPUT')).toBeInTheDocument();
    expect(screen.getByText('DONE')).toBeInTheDocument();
    expect(screen.getByText('FAILED')).toBeInTheDocument();
    expect(screen.getByText('Error: boom')).toBeInTheDocument();
  });

  it('hides finished and idle sessions until their filter is on', async () => {
    server.use(
      ...serveList([
        session({ id: 's1', title: 'Current job' }),
        session({ id: 's2', title: 'Done job', status: 'completed' }),
        session({ id: 's3', title: 'Parked job', status: 'idle' })
      ])
    );
    const user = userEvent.setup();
    renderSessions('/');
    await screen.findByText('Current job');
    expect(screen.queryByText('Done job')).not.toBeInTheDocument();
    expect(screen.queryByText('Parked job')).not.toBeInTheDocument();

    await user.click(screen.getByTestId('show-finished'));
    expect(screen.getByText('Done job')).toBeInTheDocument();
    expect(screen.getByTestId('show-finished')).toHaveTextContent('Hide finished (1)');

    await user.click(screen.getByTestId('show-idle'));
    expect(screen.getByText('Parked job')).toBeInTheDocument();
    expect(screen.getByTestId('show-idle')).toHaveTextContent('Hide idle (1)');
  });

  it('marks a child session and shows its topic', async () => {
    server.use(
      ...serveList([
        session({ id: 's1', title: 'Parent work' }),
        session({ id: 's2', title: 'Child work', parent_session_id: 's1', topic: 'auth' })
      ])
    );
    renderSessions('/');
    const row = (await screen.findByText('Child work')).closest('a');
    expect(row).not.toBeNull();
    expect(within(row!).getByTestId('child-marker')).toBeInTheDocument();
    expect(within(row!).getByTestId('session-topic')).toHaveTextContent('#auth');
  });

  it('opens the session page with its title and description when a row is clicked', async () => {
    const item = session({
      id: 's1',
      title: 'Fix login redirect',
      description: 'Investigate the redirect loop.'
    });
    server.use(...serveSession(item));
    const user = userEvent.setup();
    const { router } = renderSessions('/');
    await user.click(await screen.findByRole('link', { name: /Fix login redirect/ }));

    expect(
      await screen.findByRole('heading', { level: 1, name: 'Fix login redirect' })
    ).toBeInTheDocument();
    expect(screen.getByText('Investigate the redirect loop.')).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/sessions/s1');
  });

  it('opens the new-agent form from the "+ agent" link', async () => {
    server.use(...serveList([]));
    const user = userEvent.setup();
    renderSessions('/');
    await user.click(await screen.findByTestId('new-agent-link'));
    expect(await screen.findByRole('heading', { name: 'New agent session' })).toBeInTheDocument();
  });

  it('shows the unreachable state with a retry that recovers', async () => {
    let calls = 0;
    server.use(
      attentionRead(() => {
        calls += 1;
        if (calls === 1) return HttpResponse.error();
        return HttpResponse.json(attentionSnapshot([session({ title: 'Back online' })]));
      }),
      ...otherListReads()
    );
    const user = userEvent.setup();
    renderSessions('/');

    const failure = await screen.findByTestId('sessions-error');
    expect(within(failure).getByText('Could not load sessions')).toBeInTheDocument();
    expect(within(failure).getByText("Can't reach the Mainloop backend.")).toBeInTheDocument();

    await user.click(within(failure).getByRole('button', { name: /try again/i }));
    expect(await screen.findByText('Back online')).toBeInTheDocument();
  });

  it('shows the server error message when attention returns a 5xx', async () => {
    server.use(attentionRead(problem(500, 'Attention is down')), ...otherListReads());
    renderSessions('/');
    const failure = await screen.findByTestId('sessions-error');
    expect(within(failure).getByText('Attention is down')).toBeInTheDocument();
  });
});
