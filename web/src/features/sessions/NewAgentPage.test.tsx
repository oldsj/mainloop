import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import { connection } from '@/api';
import { server } from '@/test/msw/server';
import { session } from './test/fixtures';
import { createSession, otherListReads, problem, serveSession } from './test/handlers';
import { renderSessions } from './test/render';

afterEach(async () => {
  await connection.check();
});

const titleField = () => screen.getByTestId('agent-title') as HTMLInputElement;
const promptField = () => screen.getByTestId('agent-prompt');
const startButton = () => screen.getByTestId('agent-start');

/** Records the body of each POST /sessions and answers with a new pending session. */
function captureCreate(
  created = session({ id: 's-new', title: 'claude session', status: 'pending' })
) {
  const bodies: unknown[] = [];
  server.use(
    createSession(async ({ request }) => {
      bodies.push(await request.json());
      return HttpResponse.json(created);
    }),
    ...serveSession(created)
  );
  return bodies;
}

describe('new agent form', () => {
  it('defaults to Claude, with a title that follows the agent kind', async () => {
    server.use(...otherListReads());
    const user = userEvent.setup();
    renderSessions('/agents');
    expect(await screen.findByRole('heading', { name: 'New agent session' })).toBeInTheDocument();
    expect(screen.getByTestId('kind-claude')).toBeChecked();
    expect(titleField()).toHaveValue('claude session');

    await user.click(screen.getByTestId('kind-codex'));
    expect(titleField()).toHaveValue('codex session');
  });

  it('gives the title and prompt fields the full width of the form', async () => {
    server.use(...otherListReads());
    renderSessions('/agents');
    await screen.findByRole('heading', { name: 'New agent session' });
    expect(titleField()).toHaveClass('w-full');
    expect(promptField()).toHaveClass('w-full');
  });

  it('keeps a title the owner typed when the kind changes', async () => {
    server.use(...otherListReads());
    const user = userEvent.setup();
    renderSessions('/agents');
    await screen.findByRole('heading', { name: 'New agent session' });

    await user.clear(titleField());
    await user.type(titleField(), 'Login fix');
    await user.click(screen.getByTestId('kind-codex'));
    expect(titleField()).toHaveValue('Login fix');
  });

  it('cannot start until a prompt is written', async () => {
    server.use(...otherListReads());
    const user = userEvent.setup();
    renderSessions('/agents');
    await screen.findByRole('heading', { name: 'New agent session' });
    expect(startButton()).toBeDisabled();

    await user.type(promptField(), 'Find the bug');
    expect(startButton()).toBeEnabled();
  });

  it('starts a Claude session with the title and prompt, then opens its page', async () => {
    const bodies = captureCreate(
      session({ id: 's-new', title: 'claude session', status: 'pending' })
    );
    const user = userEvent.setup();
    const { router } = renderSessions('/agents');
    await screen.findByRole('heading', { name: 'New agent session' });

    await user.type(promptField(), 'Find the bug');
    await user.click(startButton());

    expect(
      await screen.findByRole('heading', { level: 1, name: 'claude session' })
    ).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/sessions/s-new');
    expect(bodies).toEqual([
      {
        title: 'claude session',
        description: 'Native claude agent session',
        prompt: 'Find the bug',
        agent_kind: 'claude'
      }
    ]);
  });

  it('sends the Codex kind when Codex is chosen', async () => {
    const bodies = captureCreate(
      session({ id: 's-new', title: 'codex session', status: 'pending' })
    );
    const user = userEvent.setup();
    renderSessions('/agents');
    await screen.findByRole('heading', { name: 'New agent session' });

    await user.click(screen.getByTestId('kind-codex'));
    await user.type(promptField(), 'Review the diff');
    await user.click(startButton());

    expect(
      await screen.findByRole('heading', { level: 1, name: 'codex session' })
    ).toBeInTheDocument();
    expect(bodies[0]).toMatchObject({ agent_kind: 'codex', prompt: 'Review the diff' });
  });

  it('starts on Ctrl+Enter from the prompt box', async () => {
    const bodies = captureCreate();
    const user = userEvent.setup();
    renderSessions('/agents');
    await screen.findByRole('heading', { name: 'New agent session' });

    await user.click(promptField());
    await user.keyboard('Find the bug');
    await user.keyboard('{Control>}{Enter}{/Control}');

    expect(
      await screen.findByRole('heading', { level: 1, name: 'claude session' })
    ).toBeInTheDocument();
    expect(bodies).toHaveLength(1);
  });

  it('blocks starting while the backend is unreachable, and says why', async () => {
    connection.reportFailure();
    server.use(...otherListReads());
    const user = userEvent.setup();
    renderSessions('/agents');
    await screen.findByRole('heading', { name: 'New agent session' });

    await user.type(promptField(), 'Find the bug');
    expect(startButton()).toBeDisabled();
    expect(
      screen.getByText("Backend unreachable: can't start a session right now.")
    ).toBeInTheDocument();
  });

  it('shows the backend message when the start fails', async () => {
    server.use(createSession(problem(422, 'Prompt is required')), ...otherListReads());
    const user = userEvent.setup();
    renderSessions('/agents');
    await screen.findByRole('heading', { name: 'New agent session' });

    await user.type(promptField(), 'Find the bug');
    await user.click(startButton());

    const alert = await screen.findByRole('alert');
    expect(within(alert).getByText('Prompt is required')).toBeInTheDocument();
  });
});
