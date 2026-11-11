import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ws } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import { server } from '@/test/msw/server';
import { createFakeRunner } from './fakeRunner';
import { LiveSession } from './LiveSession';
import { LIVE_SUBPROTOCOL, LIVE_TIMING, liveUrl } from './useLiveSession';

afterEach(() => {
  LIVE_TIMING.backoffMs = 500;
});

function setup(opts?: Parameters<typeof createFakeRunner>[0]) {
  const runner = createFakeRunner(opts);
  server.use(runner.handler);
  render(<LiveSession sessionId="s1" />);
  return { runner, user: userEvent.setup() };
}

const attached = () =>
  waitFor(() => {
    expect(screen.getByTestId('live-conn')).toHaveTextContent('live');
    expect(screen.getByRole('button', { name: 'Model' })).toHaveTextContent('opus-5-5');
  });
const idle = () =>
  waitFor(() => expect(screen.getByTestId('live-status')).toHaveTextContent('idle'));

async function say(user: ReturnType<typeof userEvent.setup>, text: string) {
  await user.type(screen.getByRole('textbox', { name: 'Message' }), `${text}{Enter}`);
  // Wait for the runner's echo, so a following idle check cannot pass before the turn starts.
  if (!text.startsWith('/')) await screen.findAllByText(text);
}

describe('live session', () => {
  it('builds the socket URL from the API base', () => {
    expect(liveUrl('a b', 'https://ml.example/x')).toBe(
      'ws://localhost:3000/api/sessions/a%20b/live'
    );
    expect(LIVE_SUBPROTOCOL).toBe('kagent.sdk-runner.v1');
  });

  it('streams a turn: the message, the text after a dot, the tool and its output', async () => {
    const { runner, user } = setup();
    await attached();
    expect(screen.getByRole('button', { name: 'Model' })).toHaveTextContent('opus-5-5 · high');
    await say(user, 'look around');
    expect(runner.received).toContainEqual({ k: 'user', text: 'look around' });
    expect(await screen.findByText('look around')).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByTestId('live-text')).toHaveTextContent('I will list the workspace.')
    );
    const tool = screen.getByTestId('live-tool');
    expect(tool).toHaveTextContent('Bash');
    expect(tool).toHaveTextContent('ls /data/workspace');
    await waitFor(() => expect(tool).toHaveTextContent('⎿ README.md … +2 lines'));
    await idle();
    await user.click(tool);
    expect(tool).toHaveTextContent(/README\.md\s*server\s*web/);
    // A finished turn prints nothing, as Claude Code does.
    expect(screen.queryByText(/Interrupted/)).not.toBeInTheDocument();
  });

  it('asks for permission and sends the answer', async () => {
    const { runner, user } = setup();
    await attached();
    await say(user, 'perm please');
    const card = await screen.findByTestId('live-perm');
    expect(screen.getByTestId('live-status')).toHaveTextContent('needs you');
    expect(card).toHaveTextContent('ls /data/workspace');
    await user.click(
      within(card).getByRole('button', { name: "2. Yes, and don't ask again this session" })
    );
    expect(runner.received).toContainEqual({
      k: 'perm',
      id: 'perm_1',
      behavior: 'allow',
      always: true
    });
    expect(await screen.findByText(/Bash\(ls \/data\/workspace\) →/)).toBeInTheDocument();
    expect(screen.queryByTestId('live-perm')).not.toBeInTheDocument();
    await idle();
  });

  it('waits on the A2A request when a turn defers its tool call, and refuses messages meanwhile', async () => {
    const { runner, user } = setup();
    await attached();
    await say(user, 'defer this');
    expect(
      await screen.findByText(/Waiting for your answer to the request below/)
    ).toBeInTheDocument();
    expect(screen.getByTestId('live-status')).toHaveTextContent('needs you');
    // No live prompt: the approval is the A2A task's, and a deferred turn is not an interruption.
    expect(screen.queryByTestId('live-perm')).not.toBeInTheDocument();
    expect(screen.queryByText(/Interrupted/)).not.toBeInTheDocument();
    const box = screen.getByRole('textbox', { name: 'Message' });
    expect(box).toHaveAttribute('placeholder', 'Answer the request first');
    await user.type(box, 'carry on{Enter}');
    expect(
      await screen.findByText(/waiting for an answer to a pending approval or question/)
    ).toBeInTheDocument();
    runner.answerDeferred(true);
    await waitFor(() => expect(screen.getByTestId('live-tool')).toHaveTextContent('⎿ README.md'));
    await idle();
    expect(box).toHaveAttribute('placeholder', 'Message, or / for commands');
  });

  it('shows the session requests after the transcript and needs the owner while one is pending', async () => {
    server.use(createFakeRunner().handler);
    render(
      <LiveSession
        sessionId="s1"
        needsAnswer
        requests={<div data-testid="request-card">Approve the deploy?</div>}
      />
    );
    await attached();
    expect(within(screen.getByTestId('live-log')).getByTestId('request-card')).toBeInTheDocument();
    expect(screen.getByTestId('live-status')).toHaveTextContent('needs you');
  });

  it('queues a message sent during a turn and runs it next', async () => {
    const { user } = setup({ step: 30 });
    await attached();
    await say(user, 'perm first');
    await screen.findByTestId('live-perm');
    await say(user, 'then this');
    const queued = await screen.findByText(/queued · sends when this turn ends/);
    expect(queued.parentElement).toHaveTextContent('then this');
    await user.click(screen.getByRole('button', { name: '1. Yes' }));
    await waitFor(() => expect(screen.queryByText(/queued ·/)).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getAllByTestId('live-tool')).toHaveLength(2));
    await idle();
  });

  it('interrupts a running turn from the stop button', async () => {
    const { runner, user } = setup();
    await attached();
    await say(user, 'perm wait');
    await screen.findByTestId('live-perm');
    await user.click(screen.getByRole('button', { name: 'Interrupt' }));
    expect(runner.received).toContainEqual({ k: 'interrupt' });
    expect(await screen.findByText('⎿ Interrupted')).toBeInTheDocument();
    expect(screen.getByText(/→/)).toHaveTextContent('cancelled');
    await idle();
  });

  it('switches model and effort, and cycles the permission mode', async () => {
    const { runner, user } = setup();
    await attached();
    await user.click(screen.getByRole('button', { name: 'Model' }));
    const sheet = screen.getByRole('dialog', { name: 'Model' });
    await user.click(within(sheet).getByRole('radio', { name: /Sonnet 5\.5/ }));
    expect(runner.received).toContainEqual({ k: 'model', model: 'claude-sonnet-5-5' });
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Model' })).toHaveTextContent('sonnet-5-5')
    );
    await say(user, '/model');
    await user.click(
      within(screen.getByRole('dialog', { name: 'Model' })).getByRole('radio', { name: 'max' })
    );
    expect(runner.received).toContainEqual({ k: 'effort', effort: 'max' });
    await user.click(screen.getByRole('button', { name: /default/ }));
    expect(runner.received).toContainEqual({ k: 'mode', mode: 'acceptEdits' });
    expect(await screen.findByRole('button', { name: /accept edits/ })).toBeInTheDocument();
  });

  it('rebuilds the transcript from the attach backlog', async () => {
    setup({
      backlog: [
        { k: 'user', seq: 1, uuid: 'u1', text: 'earlier', origin: 'a2a' },
        {
          k: 'sdk',
          seq: 2,
          m: {
            type: 'assistant',
            message: { id: 'm0', content: [{ type: 'text', text: 'From before.' }] }
          }
        },
        { k: 'sdk', seq: 3, m: { type: 'result', subtype: 'success', is_error: false } }
      ]
    });
    await attached();
    expect(screen.getByText('earlier')).toBeInTheDocument();
    expect(screen.getByTestId('live-text')).toHaveTextContent('From before.');
    await idle();
  });

  it('offers slash commands from the session', async () => {
    const { runner, user } = setup();
    await attached();
    await user.type(screen.getByRole('textbox', { name: 'Message' }), '/comp');
    const list = screen.getByRole('listbox', { name: 'Commands' });
    expect(within(list).getByRole('option', { name: /\/compact/ })).toBeInTheDocument();
    await user.keyboard('{Enter}');
    expect(runner.received).toContainEqual({ k: 'user', text: '/compact' });
  });

  it('says the live channel is unavailable when the runner never answers, and offers a way out', async () => {
    LIVE_TIMING.backoffMs = 5;
    let attempts = 0;
    server.use(
      ws.link(/\/sessions\/[^/]+\/live$/).addEventListener('connection', ({ client }) => {
        attempts += 1;
        client.close(1011, 'runner unreachable');
      })
    );
    let fellBack = false;
    render(<LiveSession sessionId="s1" onFallback={() => (fellBack = true)} />);
    const alert = await screen.findByTestId('live-unavailable');
    expect(alert).toHaveTextContent('Live view unavailable');
    expect(screen.getByTestId('live-conn')).toHaveTextContent('unavailable');
    expect(attempts).toBeGreaterThanOrEqual(5);
    const user = userEvent.setup();
    await user.click(within(alert).getByRole('button', { name: 'Use the chat view' }));
    expect(fellBack).toBe(true);
  });

  it('steps through sent messages with Up and Down and keeps the draft', async () => {
    const { user } = setup({
      backlog: [{ k: 'user', seq: 1, uuid: 'u0', text: 'from before', origin: 'client' }]
    });
    await attached();
    await say(user, 'first');
    await idle();
    await say(user, 'second');
    await idle();
    const box = screen.getByRole('textbox', { name: 'Message' });
    await user.type(box, 'half typed');
    await user.keyboard('{ArrowUp}');
    expect(box).toHaveValue('second');
    await user.keyboard('{ArrowUp}');
    expect(box).toHaveValue('first');
    await user.keyboard('{ArrowUp}');
    expect(box).toHaveValue('from before');
    await user.keyboard('{ArrowUp}');
    expect(box).toHaveValue('from before');
    await user.keyboard('{ArrowDown}{ArrowDown}');
    expect(box).toHaveValue('second');
    await user.keyboard('{ArrowDown}');
    expect(box).toHaveValue('half typed');
  });

  it('moves within a multi-line message instead of recalling', async () => {
    const { user } = setup();
    await attached();
    await say(user, 'sent');
    await idle();
    const box = screen.getByRole<HTMLTextAreaElement>('textbox', { name: 'Message' });
    fireEvent.change(box, { target: { value: 'line one\nline two' } });
    box.setSelectionRange(12, 12);
    fireEvent.keyDown(box, { key: 'ArrowUp' });
    expect(box).toHaveValue('line one\nline two');
    box.setSelectionRange(3, 3);
    fireEvent.keyDown(box, { key: 'ArrowUp' });
    expect(box).toHaveValue('sent');
  });

  it('recalls a sent message from the history button on a phone', async () => {
    const { user } = setup();
    await attached();
    expect(screen.queryByRole('button', { name: 'Sent messages' })).not.toBeInTheDocument();
    await say(user, 'deploy it');
    await idle();
    await say(user, 'check logs');
    await idle();
    const box = screen.getByRole('textbox', { name: 'Message' });
    await user.type(box, 'draft');
    await user.click(screen.getByRole('button', { name: 'Sent messages' }));
    const list = screen.getByRole('listbox', { name: 'Sent messages' });
    await user.click(within(list).getByRole('option', { name: 'deploy it' }));
    expect(box).toHaveValue('deploy it');
    expect(screen.queryByRole('listbox', { name: 'Sent messages' })).not.toBeInTheDocument();
    await user.keyboard('{ArrowDown}{ArrowDown}');
    expect(box).toHaveValue('draft');
  });
});
