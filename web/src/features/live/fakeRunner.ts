/**
 * A fake SDK runner behind `/sessions/{id}/live`, speaking the Client event protocol. Tests use it,
 * and so does the local stack (`VITE_MSW=1 pnpm dev`, then open a session with `?live=1`).
 *
 * Each message runs a short scripted turn: streamed text, a Bash call and its output. A message
 * containing "perm" asks for permission first and waits for the answer. A message containing "defer"
 * ends its turn on a deferred call, as an A2A turn does with a protected tool (kagent #90): the
 * runner then refuses messages until `answerDeferred` replays the call. Messages sent while a turn
 * runs are echoed as queued and picked up when the turn ends, as the real runner folds them.
 */
import { ws, type WebSocketHandler } from 'msw';

type Out = Record<string, unknown>;

export interface FakeRunner {
  handler: WebSocketHandler;
  /** Every client command received, oldest first. */
  received: Out[];
  /** Frames sent before any command, as the attach backlog. */
  backlog: Out[];
  /** Answers the deferred call through the A2A task: the call is replayed and its turn finishes. */
  answerDeferred: (allow: boolean) => void;
  /** The URL of every attach, oldest first. */
  attaches: string[];
  /** Drops every attached client, as kagent suspending the actor does. */
  drop: () => void;
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

export function createFakeRunner(opts: { step?: number; backlog?: Out[] } = {}): FakeRunner {
  const step = opts.step ?? 0;
  const link = ws.link(/\/sessions\/[^/]+\/live(\?.*)?$/);
  const runner: FakeRunner = {
    handler: null as unknown as WebSocketHandler,
    received: [],
    backlog: opts.backlog ?? [],
    answerDeferred: () => {},
    attaches: [],
    drop: () => {
      for (const c of clients.splice(0)) c.close(1001, 'actor suspended');
    }
  };
  const clients: { close: (code?: number, reason?: string) => void }[] = [];
  let seq = runner.backlog.length;
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

  runner.handler = link.addEventListener('connection', ({ client }) => {
    runner.attaches.push(client.url.toString());
    clients.push(client);
    const send = (f: Out) => client.send(JSON.stringify({ ...f, seq: ++seq, ts: Date.now() }));
    const sdk = (m: Out) => send({ k: 'sdk', m });
    let running = false;
    let pendingPerm: { id: string; resolve: (allow: boolean) => void } | null = null;
    let interrupted = false;
    let deferred: string | null = null; // the deferred tool_use id
    const queue: { uuid: string; text: string }[] = [];
    const wait = () => new Promise((r) => setTimeout(r, step));
    let n = 0;

    client.send(
      JSON.stringify({
        k: 'hello',
        v: 1,
        meta,
        backlog: runner.backlog,
        perms: [],
        runner: { state: 'ready' },
        now: Date.now()
      })
    );

    const turn = async (first: { uuid: string; text: string }) => {
      running = true;
      interrupted = false;
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
      const next = queue.shift();
      if (next) void turn(next);
      else send({ k: 'turn', state: 'idle', uuids: [] });
    };

    client.addEventListener('message', (event) => {
      let c: Out;
      try {
        c = JSON.parse(String(event.data)) as Out;
      } catch {
        return;
      }
      runner.received.push(c);
      switch (c.k) {
        case 'user': {
          if (deferred) {
            send({
              k: 'err',
              text: 'the conversation is waiting for an answer to a pending approval or question',
              ref: c.id
            });
            return;
          }
          const msg = { uuid: String(c.uuid ?? `u_${seq + 1}`), text: String(c.text ?? '') };
          send({ k: 'user', uuid: msg.uuid, text: msg.text, origin: 'client' });
          if (running) queue.push(msg);
          else void turn(msg);
          return;
        }
        case 'perm': {
          const p = pendingPerm;
          if (!p || p.id !== c.id) {
            send({ k: 'err', text: 'no such permission request', ref: c.id });
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
        case 'interrupt':
          send({ k: 'ctl', what: 'interrupt', ms: 40 });
          if (running) {
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
            finish('error_during_execution');
          }
          return;
        case 'model':
          meta.model = String(c.model);
          send({ k: 'ctl', what: 'model', model: c.model, ms: 30 });
          return;
        case 'effort':
          meta.effort = String(c.effort);
          send({ k: 'ctl', what: 'effort', effort: c.effort, ms: 5 });
          return;
        case 'mode':
          meta.permissionMode = String(c.mode);
          send({ k: 'ctl', what: 'mode', mode: c.mode, ms: 5 });
          send({ k: 'meta', meta: { permissionMode: c.mode } });
          return;
      }
    });
  });

  return runner;
}
