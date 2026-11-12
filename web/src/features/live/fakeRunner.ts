/**
 * A fake SDK runner behind `/sessions/{id}/live`, speaking the Client event protocol, and the part of
 * Mainloop that the live view sends input through (`/message`, `/stop-turn`, `/model`, `/effort`).
 * Tests use it, and so does the local stack (`VITE_MSW=1 pnpm dev`, then open a session with
 * `?live=1`).
 *
 * The runner is one process: its turns run whether or not a view is attached, and every attach
 * starts with `hello` and the frames so far. Each message runs a short scripted turn: streamed text,
 * a Bash call and its output. A message containing "perm" asks for permission first and waits for
 * the answer. A message containing "defer" ends its turn on a deferred call, as an A2A turn does with
 * a protected tool (kagent #90); Mainloop then holds messages until `answerDeferred` replays the call.
 *
 * As Mainloop does, the fake records every message under its id. A message sent during a turn joins
 * it at the next tool boundary (a steer); a slash command, or a message while the turn waits on the
 * deferred call, waits behind the turn and runs as its own. A model or effort applies from the next
 * turn. Over the socket the runner takes only the permission mode and permission answers.
 */
import { http, HttpResponse, ws, type WebSocketHandler } from 'msw';
import { API_URL } from '@/api/config';

type Out = Record<string, unknown>;
type Msg = { uuid: string; mid: string; text: string };

export interface FakeRunner {
  handler: WebSocketHandler;
  /** The socket handler and Mainloop's input endpoints, for `server.use(...handlers)`. */
  handlers: (WebSocketHandler | ReturnType<typeof http.post>)[];
  /** Every client command received over the socket, oldest first. */
  received: Out[];
  /** The text of every message the runner took from Mainloop, in the order it echoed them. */
  echoed: string[];
  /** Every input sent through Mainloop, oldest first: the path's last part and the body. */
  posted: { what: string; body: Out }[];
  /** Frames sent before any command, as the attach backlog. */
  backlog: Out[];
  /** Answers the deferred call through the A2A task: the call is replayed and its turn finishes. */
  answerDeferred: (allow: boolean) => void;
  /** The URL of every attach, oldest first. */
  attaches: string[];
  /** Drops every attached client, as kagent suspending the actor does. */
  drop: () => void;
  /** Mainloop answers the next `n` message sends with a server error, as a failed request. */
  failSends: (n: number) => void;
}

const MODELS = [
  {
    value: 'claude-opus-5-5',
    displayName: 'Opus 5.5',
    description: 'Most capable',
    supportedEffortLevels: ['low', 'medium', 'high', 'xhigh', 'max']
  },
  { value: 'claude-sonnet-5-5', displayName: 'Sonnet 5.5', description: 'Fast and capable' }
];

export function createFakeRunner(
  opts: {
    step?: number;
    backlog?: Out[];
    /** Whether Mainloop's endpoints answer for a session; the others fall through. All by default. */
    owns?: (sessionId: string, runner: FakeRunner) => boolean;
  } = {}
): FakeRunner {
  const step = opts.step ?? 0;
  const link = ws.link(/\/sessions\/[^/]+\/live(\?.*)?$/);
  const clients: {
    send: (data: string) => void;
    close: (code?: number, reason?: string) => void;
  }[] = [];
  let failing = 0;
  const runner: FakeRunner = {
    handler: null as unknown as WebSocketHandler,
    handlers: [],
    received: [],
    echoed: [],
    posted: [],
    backlog: opts.backlog ?? [],
    answerDeferred: () => {},
    attaches: [],
    drop: () => {
      for (const c of clients.splice(0)) c.close(1001, 'actor suspended');
    },
    failSends: (n) => {
      failing = n;
    }
  };
  let seq = runner.backlog.length;
  const log: Out[] = [];
  const meta = {
    sessionId: 'f00dcafe-0000-4000-8000-000000000001',
    model: 'claude-opus-5-5',
    permissionMode: 'default',
    effort: 'high',
    cwd: '/data/workspace',
    version: '2.1.260',
    profile: 'managed',
    models: MODELS,
    commands: [
      { name: 'compact', description: 'Clear history but keep a summary' },
      { name: 'clear', description: 'Start a new conversation' },
      { name: 'model', description: 'Set the model' }
    ]
  };
  // Mainloop's overrides for the next turn.
  const next: { model?: string; effort?: string } = {};

  const send = (f: Out) => {
    const frame = { ...f, seq: ++seq, ts: Date.now() };
    log.push(frame);
    for (const c of clients) c.send(JSON.stringify(frame));
  };
  const sdk = (m: Out) => send({ k: 'sdk', m });
  let running = false;
  let pendingPerm: { id: string; resolve: (allow: boolean) => void } | null = null;
  let interrupted = false;
  let deferred: string | null = null; // the deferred tool_use id
  const held: Msg[] = []; // Mainloop's queue: messages waiting for the turn to end
  const steers: Msg[] = []; // messages joining the running turn
  const recorded = new Map<string, string>(); // message id -> status
  const wait = () => new Promise((r) => setTimeout(r, step));
  let n = 0;
  let u = 0;

  const echo = (m: Msg) => {
    runner.echoed.push(m.text);
    send({ k: 'user', uuid: m.uuid, mid: m.mid, text: m.text, origin: 'a2a' });
  };

  const turn = async (first: Msg) => {
    running = true;
    interrupted = false;
    if (next.model || next.effort) {
      Object.assign(meta, next);
      send({ k: 'meta', meta: { ...next } });
      delete next.model;
      delete next.effort;
    }
    const uuids = [first.uuid];
    send({ k: 'turn', state: 'running', uuids });
    const id = `msg_${++n}`;
    const tool = `toolu_${n}`;
    sdk({
      type: 'stream_event',
      event: { type: 'message_start', message: { id } },
      user_message_uuids: uuids
    });
    sdk({
      type: 'stream_event',
      event: { type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } }
    });
    for (const w of ['I will ', 'list the ', 'workspace.']) {
      await wait();
      sdk({
        type: 'stream_event',
        event: { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: w } }
      });
    }
    const input = { command: 'ls /data/workspace', description: 'List files' };
    sdk({
      type: 'stream_event',
      event: {
        type: 'content_block_start',
        index: 1,
        content_block: { type: 'tool_use', id: tool, name: 'Bash', input: {} }
      }
    });
    sdk({
      type: 'assistant',
      message: {
        id,
        content: [
          { type: 'text', text: 'I will list the workspace.' },
          { type: 'tool_use', id: tool, name: 'Bash', input }
        ]
      }
    });
    if (first.text.includes('defer')) {
      await wait();
      sdk({ type: 'result', subtype: 'success', stop_reason: 'tool_deferred', is_error: false });
      running = false;
      deferred = tool;
      send({ k: 'turn', state: 'idle', uuids: [] });
      return;
    }
    let allowed = true;
    if (first.text.includes('perm')) {
      const permId = `perm_${n}`;
      allowed = await new Promise<boolean>((resolve) => {
        pendingPerm = { id: permId, resolve };
        send({
          k: 'perm',
          id: permId,
          toolName: 'Bash',
          input,
          title: 'Bash command',
          description: 'List files',
          suggestions: [{ type: 'addRules' }],
          toolUseID: tool
        });
      });
    }
    await wait();
    if (interrupted) return;
    sdk({
      type: 'user',
      message: {
        content: [
          {
            type: 'tool_result',
            tool_use_id: tool,
            is_error: !allowed,
            content: allowed ? 'README.md\nserver\nweb' : 'Permission denied by the owner'
          }
        ]
      }
    });
    // The tool boundary: messages sent meanwhile join the turn here.
    const joined = steers.splice(0);
    if (joined.length) {
      send({ k: 'turn', state: 'running', uuids: joined.map((m) => m.uuid) });
      sdk({
        type: 'assistant',
        message: {
          id: `${id}_steer`,
          content: [{ type: 'text', text: `Noted: ${joined.map((m) => m.text).join('; ')}` }]
        }
      });
    }
    await wait();
    if (interrupted) return;
    finish('success');
  };

  runner.answerDeferred = (allow: boolean) => {
    const tool = deferred;
    if (!tool) return;
    deferred = null;
    running = true;
    send({ k: 'turn', state: 'running', uuids: [] });
    sdk({
      type: 'user',
      message: {
        content: [
          {
            type: 'tool_result',
            tool_use_id: tool,
            is_error: !allow,
            content: allow ? 'README.md\nserver\nweb' : 'Denied by the owner'
          }
        ]
      }
    });
    finish('success');
  };

  const finish = (subtype: string) => {
    sdk({
      type: 'result',
      subtype,
      is_error: subtype !== 'success',
      duration_ms: 1200,
      num_turns: 1
    });
    running = false;
    // Steers the turn never reached run next, ahead of anything sent after them.
    held.unshift(...steers.splice(0));
    const following = held.shift();
    if (following) {
      echo(following);
      void turn(following);
    } else send({ k: 'turn', state: 'idle', uuids: [] });
  };

  const stop = () => {
    if (!running) return false;
    send({ k: 'ctl', what: 'interrupt', ms: 40 });
    interrupted = true;
    if (pendingPerm) {
      send({
        k: 'perm_done',
        id: pendingPerm.id,
        behavior: 'deny',
        always: false,
        aborted: true,
        by: 'client'
      });
      pendingPerm = null;
    }
    // A stopped turn takes its steers with it, as Esc does in Claude Code.
    steers.splice(0);
    finish('error_during_execution');
    return true;
  };

  const message = (body: Out): string => {
    const mid = String(body.message_id ?? `m_${u + 1}`);
    const seen = recorded.get(mid);
    if (seen) return seen;
    const m: Msg = { uuid: `u_${++u}`, mid, text: String(body.message ?? '') };
    let status = 'ok';
    if (deferred || (running && (!body.steer || m.text.startsWith('/')))) {
      status = 'queued';
      held.push(m);
    } else if (running) {
      status = 'steered';
      steers.push(m);
      echo(m);
    } else {
      echo(m);
      void turn(m);
    }
    recorded.set(mid, status);
    return status;
  };

  const path = (what: string) => `${API_URL}/sessions/:id/${what}`;
  const owns = (params: Record<string, unknown>) =>
    !opts.owns || opts.owns(String(params.id), runner);
  const posted = async (what: string, request: Request) => {
    const body = (await request.json().catch(() => ({}))) as Out;
    runner.posted.push({ what, body });
    return body;
  };

  runner.handler = link.addEventListener('connection', ({ client }) => {
    runner.attaches.push(client.url.toString());
    clients.push(client);
    client.addEventListener('close', () => {
      const i = clients.indexOf(client);
      if (i >= 0) clients.splice(i, 1);
    });
    client.send(
      JSON.stringify({
        k: 'hello',
        v: 1,
        meta,
        backlog: [...runner.backlog, ...log],
        perms: pendingPerm ? log.filter((f) => f.k === 'perm' && f.id === pendingPerm?.id) : [],
        runner: { state: 'ready' },
        now: Date.now()
      })
    );

    client.addEventListener('message', (event) => {
      let c: Out;
      try {
        c = JSON.parse(String(event.data)) as Out;
      } catch {
        return;
      }
      runner.received.push(c);
      switch (c.k) {
        case 'perm': {
          const p = pendingPerm;
          if (!p || p.id !== c.id) {
            client.send(
              JSON.stringify({ k: 'err', text: 'no such permission request', ref: c.id })
            );
            return;
          }
          pendingPerm = null;
          send({
            k: 'perm_done',
            id: p.id,
            behavior: c.behavior,
            always: Boolean(c.always),
            by: 'client'
          });
          p.resolve(c.behavior === 'allow');
          return;
        }
        case 'mode':
          meta.permissionMode = String(c.mode);
          send({ k: 'ctl', what: 'mode', mode: c.mode, ms: 5 });
          send({ k: 'meta', meta: { permissionMode: c.mode } });
          return;
        default:
          client.send(
            JSON.stringify({ k: 'err', text: `send ${String(c.k)} through Mainloop`, ref: c.id })
          );
      }
    });
  });

  runner.handlers = [
    runner.handler,
    http.post(path('message'), async ({ request, params }) => {
      if (!owns(params)) return;
      const body = await posted('message', request);
      if (failing > 0) {
        failing -= 1;
        return HttpResponse.json({ detail: 'database unavailable' }, { status: 503 });
      }
      const status = message(body);
      return HttpResponse.json(
        { status, message_id: String(body.message_id) },
        { status: status === 'ok' ? 200 : 202 }
      );
    }),
    http.post(path('stop-turn'), async ({ request, params }) => {
      if (!owns(params)) return;
      await posted('stop-turn', request);
      return HttpResponse.json({ status: stop() ? 'stopped' : 'no_open_turn' });
    }),
    http.post(path('model'), async ({ request, params }) => {
      if (!owns(params)) return;
      const body = await posted('model', request);
      next.model = String(body.model);
      return HttpResponse.json({ session_id: 's1', model: body.model, effort: null });
    }),
    http.post(path('effort'), async ({ request, params }) => {
      if (!owns(params)) return;
      const body = await posted('effort', request);
      next.effort = String(body.effort);
      return HttpResponse.json({ session_id: 's1', model: null, effort: body.effort });
    })
  ];

  return runner;
}
