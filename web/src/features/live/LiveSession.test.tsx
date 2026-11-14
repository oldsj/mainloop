import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render as rtlRender, screen, waitFor, within } from '@testing-library/react';
import type { ReactNode } from 'react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse, ws } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { API_URL } from '@/api/config';
import { server } from '@/test/msw/server';
import { useSessionRequests } from '@/features/hitl/session-requests';
import { createFakeRunner, type FakeRunner } from './fakeRunner';
import { LiveSession } from './LiveSession';
import { LIVE_SUBPROTOCOL, LIVE_TIMING, liveDeps, liveUrl, type LiveDeps } from './useLiveSession';

/** What Mainloop reports about session s1: kagent's current_active (null: no association) and its record. */
const world: {
  active: boolean | null;
  messages: { role: 'user' | 'assistant'; content: string }[];
  nativeReads: number;
} = { active: true, messages: [], nativeReads: 0 };

/**
 * The reads outside the live channel, from `world`; the real ones are tested once, below. Input goes
 * through the real client to the fake runner's Mainloop endpoints.
 */
const deps: LiveDeps = {
  ...liveDeps,
  awake: async () => {
    world.nativeReads += 1;
    return world.active;
  },
  history: async () => world.messages
};

beforeEach(() => {
  world.active = true;
  world.messages = [];
  world.nativeReads = 0;
  LIVE_TIMING.settleMs = 20;
  LIVE_TIMING.asleepPollMs = 40;
});

afterEach(() => {
  LIVE_TIMING.backoffMs = 500;
  LIVE_TIMING.settleMs = 1500;
  LIVE_TIMING.asleepPollMs = 10_000;
  LIVE_TIMING.idleDetachMs = 120_000;
  LIVE_TIMING.hiddenDetachMs = 15_000;
  setHidden(false);
});

// The row shows one status word. The channel's own state is on the view, as `data-conn`; while the
// channel is detached and nothing waits on the owner, the word is that state.
const conn = (state: 'asleep' | 'paused') =>
  waitFor(() => {
    expect(screen.getByTestId('live-session')).toHaveAttribute('data-conn', state);
    expect(screen.getByTestId('live-status')).toHaveTextContent(state);
  });
const pause = (ms: number) => new Promise((r) => setTimeout(r, ms));

let current: FakeRunner;

const render = (ui: ReactNode) =>
  rtlRender(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      {ui}
    </QueryClientProvider>
  );

/** The session's request cards by id: pending ones and this browser's answered ones. */
function RequestCards({ skip }: { skip: readonly string[] }) {
  const { shown } = useSessionRequests('s1', skip);
  return (
    <ul data-testid="request-cards">
      {shown.map((id) => (
        <li key={id}>{id}</li>
      ))}
    </ul>
  );
}

/** The live view as the session page places it, with the session's requests listed by id. */
function WithRequests() {
  const { pending } = useSessionRequests('s1');
  return (
    <LiveSession
      sessionId="s1"
      deps={deps}
      pending={pending}
      requests={(skip) => <RequestCards skip={skip} />}
    />
  );
}

function setup(opts?: Parameters<typeof createFakeRunner>[0]) {
  const runner = createFakeRunner(opts);
  current = runner;
  server.use(...runner.handlers);
  render(<WithRequests />);
  return { runner, user: userEvent.setup() };
}

function setHidden(hidden: boolean) {
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden });
  document.dispatchEvent(new Event('visibilitychange'));
}

const attached = () =>
  waitFor(() => {
    expect(screen.getByTestId('live-session')).toHaveAttribute('data-conn', 'attached');
    expect(screen.getByTestId('session-controls')).toHaveTextContent('opus-5-5');
  });
const idle = () =>
  waitFor(() => expect(screen.getByTestId('live-status')).toHaveTextContent('ready'));

/** A message the view sent through Mainloop, with this text. */
const message = (text: string) => ({
  what: 'message',
  body: { message: text, message_id: expect.any(String), steer: true }
});

async function say(user: ReturnType<typeof userEvent.setup>, text: string, echo = true) {
  await user.type(screen.getByRole('textbox', { name: 'Message' }), `${text}{Enter}`);
  await waitFor(() => expect(current.posted).toContainEqual(message(text)));
  // Wait for the runner's echo, so a following idle check cannot pass before the turn starts.
  if (echo) await waitFor(() => expect(current.echoed).toContain(text));
  await screen.findAllByText(text);
}

describe('live session', () => {
  it('builds the socket URL from the API base', () => {
    expect(liveUrl('a b', 'https://ml.example/x')).toBe(
      'ws://localhost:3000/api/sessions/a%20b/live'
    );
    expect(LIVE_SUBPROTOCOL).toBe('kagent.sdk-runner.v1');
  });

  describe('a long message', () => {
    // jsdom has no layout. A clamped box (line-clamp) reports a ten-line height and its full
    // content height, as a browser does; an unclamped one reports them equal.
    const heights = (full: number) => {
      const scroll = vi.spyOn(HTMLElement.prototype, 'scrollHeight', 'get');
      const client = vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get');
      scroll.mockImplementation(function (this: HTMLElement) {
        return this.className.includes('whitespace-pre-wrap') ? full : 0;
      });
      client.mockImplementation(function (this: HTMLElement) {
        if (!this.className.includes('whitespace-pre-wrap')) return 0;
        return this.className.includes('line-clamp-10') ? Math.min(full, 200) : full;
      });
      return () => {
        scroll.mockRestore();
        client.mockRestore();
      };
    };

    it('shows its first lines with Show all, and opens and closes', async () => {
      const restore = heights(900);
      try {
        const { user } = setup();
        await attached();
        await say(user, 'a long report');
        const row = screen.getByTestId('live-user');
        const toggle = await within(row).findByRole('button', { name: 'Show all' });
        expect(toggle).toHaveAttribute('aria-expanded', 'false');
        expect(within(row).getByText(/a long report/)).toHaveClass('line-clamp-10');
        await user.click(toggle);
        expect(within(row).getByText(/a long report/)).not.toHaveClass('line-clamp-10');
        const less = within(row).getByRole('button', { name: 'Show less' });
        expect(less).toHaveAttribute('aria-expanded', 'true');
        await user.click(less);
        expect(within(row).getByText(/a long report/)).toHaveClass('line-clamp-10');
      } finally {
        restore();
      }
    });

    it('offers nothing when the message fits', async () => {
      const restore = heights(120);
      try {
        const { user } = setup();
        await attached();
        await say(user, 'short');
        expect(
          within(screen.getByTestId('live-user')).queryByRole('button')
        ).not.toBeInTheDocument();
      } finally {
        restore();
      }
    });

    it('never cuts a report that names what the owner must do', async () => {
      const restore = heights(900);
      try {
        const { user } = setup();
        await attached();
        // Typed as one line; the store keeps the text as sent.
        await user.click(screen.getByRole('textbox', { name: 'Message' }));
        await user.paste('report body\nOwner action needed: decide the schema');
        await user.keyboard('{Enter}');
        const row = await screen.findByTestId('live-user');
        expect(within(row).getByText(/Owner action needed/)).not.toHaveClass('line-clamp-10');
        expect(within(row).queryByRole('button')).not.toBeInTheDocument();
      } finally {
        restore();
      }
    });
  });

  it('keeps the transcript and the composer to the reading width, inside a full-width scroller', async () => {
    setup();
    await attached();
    const log = screen.getByTestId('live-log');
    expect(log).not.toHaveClass('max-w-measure');
    expect(log.firstElementChild).toHaveClass('max-w-measure', 'mx-auto');
    expect(
      screen.getByRole('textbox', { name: 'Message' }).closest('.max-w-measure')
    ).not.toBeNull();
  });

  it('streams a turn: the message, the text after a dot, the tool and its output', async () => {
    const { runner, user } = setup();
    await attached();
    expect(screen.getByTestId('session-controls')).toHaveTextContent('opus-5-5 · high');
    await say(user, 'look around');
    // Through Mainloop, never the socket; the runner's echo replaces the message shown at once.
    expect(runner.received).toEqual([]);
    await waitFor(() => expect(screen.getAllByText('look around')).toHaveLength(1));
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

  it('answers a permission prompt through its session request, shown once', async () => {
    const { runner, user } = setup();
    await attached();
    await say(user, 'perm please');
    const card = await screen.findByTestId('live-perm');
    expect(screen.getByTestId('live-status')).toHaveTextContent('needs you');
    expect(card).toHaveTextContent('ls /data/workspace');
    // The prompt is the request: its card is not listed again below the transcript.
    const yes = await within(card).findByRole('button', { name: '1. Yes' });
    expect(within(card).getByRole('button', { name: '2. No' })).toBeInTheDocument();
    expect(screen.getByTestId('request-cards')).toBeEmptyDOMElement();
    await user.click(yes);
    await waitFor(() =>
      expect(runner.responses).toEqual([
        { requestId: expect.stringMatching(/^req_\d+_perm_1$/), approved: true }
      ])
    );
    expect(runner.received).toEqual([]);
    expect(await screen.findByText(/Bash\(ls \/data\/workspace\) →/)).toHaveTextContent('Yes');
    expect(screen.queryByTestId('live-perm')).not.toBeInTheDocument();
    await idle();
    // The prompt shows the answer, so the request's receipt is not listed below it either.
    expect(screen.getByTestId('request-cards')).toBeEmptyDOMElement();
  });

  it('takes back an answer Mainloop did not record, so it can be given again', async () => {
    const { runner, user } = setup();
    await attached();
    await say(user, 'perm please');
    const card = await screen.findByTestId('live-perm');
    server.use(
      http.post(
        `${API_URL}/hitl/:id/respond`,
        () => HttpResponse.json({ detail: 'The request changed.' }, { status: 409 }),
        { once: true }
      )
    );
    await user.click(await within(card).findByRole('button', { name: '1. Yes' }));
    expect(await screen.findByText(/Not answered: .*The request changed/)).toBeInTheDocument();
    expect(runner.responses).toEqual([]);
    expect(
      within(screen.getByTestId('live-perm')).getByRole('button', { name: '1. Yes' })
    ).toBeInTheDocument();
  });

  it('shows a prompt without choices until Mainloop has raised its request, then denies it', async () => {
    const { runner, user } = setup();
    runner.scanned = false;
    await attached();
    await say(user, 'perm please');
    const card = await screen.findByTestId('live-perm');
    expect(card).toHaveTextContent('Waiting for Mainloop to record the request');
    expect(within(card).queryByRole('button')).not.toBeInTheDocument();
    runner.scanned = true;
    await user.click(await within(card).findByRole('button', { name: '2. No' }, { timeout: 3000 }));
    await waitFor(() =>
      expect(runner.responses).toEqual([
        { requestId: expect.stringMatching(/^req_\d+_perm_1$/), approved: false }
      ])
    );
    expect(await screen.findByText(/Bash\(ls \/data\/workspace\) →/)).toHaveTextContent('No');
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
    // Mainloop holds a message while the turn waits on its request, and runs it once answered.
    await say(user, 'carry on', false);
    expect(await screen.findByText(/queued · sends when this turn ends/)).toBeInTheDocument();
    expect(runner.echoed).not.toContain('carry on');
    runner.answerDeferred(true);
    await waitFor(() => expect(runner.echoed).toContain('carry on'));
    await waitFor(() => expect(screen.getAllByTestId('live-tool')).toHaveLength(2));
    await idle();
    expect(box).toHaveAttribute('placeholder', 'Message, or / for commands');
  });

  it('shows the session requests after the transcript and needs the owner while one is pending', async () => {
    server.use(createFakeRunner().handler);
    render(
      <LiveSession
        sessionId="s1"
        pending={['deploy']}
        requests={() => <div data-testid="request-card">Approve the deploy?</div>}
      />
    );
    await attached();
    expect(within(screen.getByTestId('live-log')).getByTestId('request-card')).toBeInTheDocument();
    expect(screen.getByTestId('live-status')).toHaveTextContent('needs you');
  });

  it('folds a message sent during a turn into that turn at its next tool boundary', async () => {
    const { user } = setup({ step: 30 });
    await attached();
    await say(user, 'perm first');
    await screen.findByTestId('live-perm');
    await say(user, 'then this');
    const queued = await screen.findByText(/queued · joins this turn/);
    expect(queued.parentElement).toHaveTextContent('then this');
    await user.click(await screen.findByRole('button', { name: '1. Yes' }));
    expect(await screen.findByText('Noted: then this')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText(/queued ·/)).not.toBeInTheDocument());
    await idle();
    // One turn: the message joined it rather than running after it.
    expect(screen.getAllByTestId('live-tool')).toHaveLength(1);
    expect(screen.getAllByText('then this')).toHaveLength(1);
  });

  it('runs a slash command sent during a turn as the next turn', async () => {
    const { user } = setup({ step: 30 });
    await attached();
    await say(user, 'perm first');
    await screen.findByTestId('live-perm');
    await user.type(screen.getByRole('textbox', { name: 'Message' }), '/compact {Enter}');
    expect(await screen.findByText(/queued · sends when this turn ends/)).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: '1. Yes' }));
    await waitFor(() => expect(screen.getAllByTestId('live-tool')).toHaveLength(2));
    await idle();
  });

  it('retries a send under the same id and says when it was not recorded', async () => {
    LIVE_TIMING.backoffMs = 5;
    const { runner, user } = setup();
    await attached();
    runner.failSends(1);
    await say(user, 'once only');
    await idle();
    const sends = runner.posted.filter((p) => p.what === 'message');
    expect(sends).toHaveLength(2);
    expect(sends[1]?.body.message_id).toBe(sends[0]?.body.message_id);
    runner.failSends(3);
    await user.type(screen.getByRole('textbox', { name: 'Message' }), 'lost{Enter}');
    expect(await screen.findByText(/Not sent: database unavailable/)).toBeInTheDocument();
    expect(runner.echoed).not.toContain('lost');
  });

  it('interrupts a running turn from the stop button', async () => {
    const { runner, user } = setup();
    await attached();
    await say(user, 'perm wait');
    await screen.findByTestId('live-perm');
    await user.click(screen.getByRole('button', { name: 'Interrupt' }));
    await waitFor(() => expect(runner.posted.map((p) => p.what)).toContain('stop-turn'));
    expect(await screen.findByText('⎿ Interrupted')).toBeInTheDocument();
    expect(screen.getByText(/→/)).toHaveTextContent('cancelled');
    await idle();
  });

  it('switches model and effort, and cycles the permission mode', async () => {
    const { runner, user } = setup();
    await attached();
    await user.click(screen.getByTestId('session-controls'));
    const sheet = screen.getByRole('dialog', { name: 'Session controls' });
    await user.click(within(sheet).getByRole('radio', { name: /Sonnet 5\.5/ }));
    await waitFor(() =>
      expect(runner.posted).toContainEqual({ what: 'model', body: { model: 'claude-sonnet-5-5' } })
    );
    expect(await screen.findByText('Set model to claude-sonnet-5-5')).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByTestId('session-controls')).toHaveTextContent('sonnet-5-5')
    );
    await user.type(screen.getByRole('textbox', { name: 'Message' }), '/model{Enter}');
    await user.click(
      within(screen.getByRole('dialog', { name: 'Session controls' })).getByRole('radio', {
        name: 'max'
      })
    );
    await waitFor(() =>
      expect(runner.posted).toContainEqual({ what: 'effort', body: { effort: 'max' } })
    );
    // /effort with a level sets it without the picker; neither command reaches Claude.
    await user.type(screen.getByRole('textbox', { name: 'Message' }), '/effort low{Enter}');
    await waitFor(() =>
      expect(runner.posted).toContainEqual({ what: 'effort', body: { effort: 'low' } })
    );
    expect(runner.posted.filter((p) => p.what === 'message')).toEqual([]);
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
    await waitFor(() => expect(runner.posted).toContainEqual(message('/compact')));
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
    render(<LiveSession sessionId="s1" deps={deps} onFallback={() => (fellBack = true)} />);
    const alert = await screen.findByTestId('live-unavailable');
    expect(alert).toHaveTextContent('Live view unavailable');
    expect(screen.getByTestId('live-status')).toHaveTextContent('unavailable');
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

  describe('sleep (#348)', () => {
    it('stays asleep when the actor sleeps, and never reattaches on its own', async () => {
      const { runner } = setup();
      await attached();
      world.active = false;
      runner.drop();
      await conn('asleep');
      // One row, no banner: the word, a Wake now button, and the composer says what a message does.
      expect(screen.queryByTestId('live-asleep')).toBeNull();
      expect(screen.getByRole('button', { name: 'Wake now' })).toBeInTheDocument();
      expect(screen.getByRole('textbox', { name: 'Message' })).toHaveAttribute(
        'placeholder',
        'Message to wake the session'
      );
      await pause(300); // several asleep polls
      expect(runner.attaches).toHaveLength(1);
      expect(world.nativeReads).toBeGreaterThan(2);
    });

    it('still reconnects after a drop while the session is awake', async () => {
      const { runner } = setup();
      await attached();
      runner.drop();
      await waitFor(() => expect(runner.attaches).toHaveLength(2));
      expect(runner.attaches[1]).not.toContain('wake=');
      await attached();
    });

    it("opens a sleeping session without attaching, showing Mainloop's record", async () => {
      world.active = false;
      world.messages = [
        { role: 'user', content: 'what changed?' },
        { role: 'assistant', content: 'Two files changed.' }
      ];
      const { runner } = setup();
      await conn('asleep');
      expect(await screen.findByText('what changed?')).toBeInTheDocument();
      expect(screen.getByText('Two files changed.')).toBeInTheDocument();
      expect(screen.getByRole('textbox', { name: 'Message' })).toHaveAttribute(
        'placeholder',
        'Message to wake the session'
      );
      await pause(200);
      expect(runner.attaches).toHaveLength(0);
    });

    it('wakes on a message: it attaches with wake=1 and sends the message once the runner is up', async () => {
      world.active = false;
      const { runner, user } = setup();
      await conn('asleep');
      await user.type(screen.getByRole('textbox', { name: 'Message' }), 'carry on{Enter}');
      await waitFor(() => expect(runner.posted).toContainEqual(message('carry on')));
      expect(runner.attaches).toHaveLength(1);
      expect(runner.attaches[0]).toContain('wake=1');
      await attached();
    });

    it('wakes from the button', async () => {
      world.active = false;
      const { runner, user } = setup();
      await conn('asleep');
      await user.click(screen.getByRole('button', { name: 'Wake now' }));
      await attached();
      expect(runner.attaches[0]).toContain('wake=1');
    });

    it('attaches without waking once something else woke the session', async () => {
      world.active = false;
      const { runner } = setup();
      await conn('asleep');
      world.active = true; // an A2A turn, or an answered request replayed
      await attached();
      expect(runner.attaches).toHaveLength(1);
      expect(runner.attaches[0]).not.toContain('wake=');
    });

    it('detaches when idle so the actor can sleep, and waits for it to sleep before attaching again', async () => {
      LIVE_TIMING.idleDetachMs = 60;
      const { runner } = setup();
      await attached();
      await conn('paused');
      expect(screen.getByRole('button', { name: 'Resume' })).toBeInTheDocument();
      expect(screen.getByRole('textbox', { name: 'Message' })).toHaveAttribute(
        'placeholder',
        'Message to resume the session'
      );
      await pause(200); // still awake: the view does not take the actor back
      expect(runner.attaches).toHaveLength(1);
      world.active = false;
      await conn('asleep');
      world.active = true; // something else woke it
      await attached();
      expect(runner.attaches).toHaveLength(2);
      expect(runner.attaches[1]).not.toContain('wake=');
    });

    it('resumes from the button in the row', async () => {
      LIVE_TIMING.idleDetachMs = 60;
      const { runner, user } = setup();
      await attached();
      await conn('paused');
      await user.click(screen.getByRole('button', { name: 'Resume' }));
      await attached();
      expect(runner.attaches[1]).toContain('wake=1');
    });

    it('stays attached while a turn runs, and detaches once it ends', async () => {
      LIVE_TIMING.idleDetachMs = 60;
      const { runner, user } = setup({ step: 120 });
      await attached();
      await user.type(screen.getByRole('textbox', { name: 'Message' }), 'hello{Enter}');
      await waitFor(() => expect(screen.getByTestId('live-status')).not.toHaveTextContent('ready'));
      await pause(250);
      expect(screen.getByTestId('live-session')).toHaveAttribute('data-conn', 'attached');
      await conn('paused');
      expect(runner.attaches).toHaveLength(1);
    });

    it('detaches sooner when the page is hidden', async () => {
      LIVE_TIMING.hiddenDetachMs = 30;
      setup();
      await attached();
      setHidden(true);
      await conn('paused');
    });

    it('treats an unknown association as awake', async () => {
      world.active = null;
      const { runner } = setup();
      await attached();
      expect(runner.attaches[0]).not.toContain('wake=');
    });

    it('puts the record above the live backlog without showing a turn twice', async () => {
      world.messages = [
        { role: 'user', content: 'first question' },
        { role: 'assistant', content: 'First answer.' },
        { role: 'user', content: 'second question' },
        { role: 'assistant', content: 'Second answer.' }
      ];
      setup({
        backlog: [
          { k: 'user', text: 'second question', uuid: 'u2' },
          {
            k: 'sdk',
            m: {
              type: 'assistant',
              message: { id: 'a2', content: [{ type: 'text', text: 'Second answer.' }] }
            }
          },
          { k: 'turn', state: 'idle' }
        ]
      });
      await attached();
      await screen.findByText('first question');
      expect(screen.getAllByText('second question')).toHaveLength(1);
      expect(screen.getAllByText('Second answer.')).toHaveLength(1);
      const log = screen.getByTestId('live-log').textContent ?? '';
      expect(log.indexOf('First answer.')).toBeLessThan(log.indexOf('second question'));
    });
  });

  it('reads asleep from the runtime association and the record from the conversation', async () => {
    let active: boolean | undefined = false;
    server.use(
      http.get(`${API_URL}/sessions/:id/native`, () =>
        HttpResponse.json({
          preparation_observation:
            active === undefined
              ? {}
              : {
                  runtime_association: {
                    generation_id: 'g',
                    actor_uid: 'a',
                    phase: 'Running',
                    current_active: active
                  }
                }
        })
      ),
      http.get(`${API_URL}/sessions/:id/conversation`, () =>
        HttpResponse.json({
          session: { id: 's1' },
          messages: [{ id: 'm1', conversation_id: 'c', role: 'user', content: 'hi' }]
        })
      )
    );
    expect(await liveDeps.awake('s1')).toBe(false);
    active = true;
    expect(await liveDeps.awake('s1')).toBe(true);
    active = undefined;
    expect(await liveDeps.awake('s1')).toBeNull();
    expect(await liveDeps.history('s1')).toEqual([{ role: 'user', content: 'hi' }]);
  });
});
