import { useCallback, useEffect, useRef, useState } from 'react';
import { API_URL } from '@/api/config';
import { fetchClient } from '@/api/client';
import { errorDetail } from '@/api/errors';
import type { components } from '@/api/schema.gen';
import { initialState, LiveModel, type Frame, type HistoryMessage, type LiveState } from './store';

export const LIVE_SUBPROTOCOL = 'kagent.sdk-runner.v1';

/**
 * `asleep`: kagent reports the session's actor asleep, and nothing attaches until the owner acts
 * (sends a message or presses wake) or something else wakes it (an A2A turn, an answered request).
 * `paused`: the view detached itself, idle or hidden, so the actor can sleep; it is asleep once
 * kagent says so. `waking`: the owner asked, and the attach that wakes it is under way.
 */
export type Conn =
  | 'connecting'
  | 'attached'
  | 'reconnecting'
  | 'paused'
  | 'asleep'
  | 'waking'
  | 'unavailable';

/**
 * Failed attaches in a row, with no `hello` between them, after which the view says the live
 * channel is unavailable (about 15 s with the default backoff). Waking a sleeping session answers 503
 * for a while, so a few failures are normal. It keeps retrying every 8 s after that.
 */
export const UNAVAILABLE_AFTER = 5;

/**
 * `backoffMs` is the first retry delay; it doubles up to 16 times this. `settleMs` is the least wait
 * after an attached socket drops, so kagent can report a suspend before the view decides whether to
 * reattach. `asleepPollMs` is how often an asleep view reads whether something else woke the
 * session. An attached client keeps the runner busy, so its actor never sleeps: the view detaches
 * after `idleDetachMs` with nothing happening (no turn running, no prompt unanswered, nothing
 * received, sent or typed), or `hiddenDetachMs` after the page is hidden. Tests shorten them.
 */
export const LIVE_TIMING = {
  backoffMs: 500,
  settleMs: 1500,
  asleepPollMs: 10_000,
  idleDetachMs: 120_000,
  hiddenDetachMs: 15_000
};

/** How Mainloop took a message: run now, queued behind the open turn, or joining the running turn. */
export type SendStatus = 'ok' | 'queued' | 'steered';

/**
 * What the view does through Mainloop, outside the live channel. The reads never wake the session.
 * Input goes through Mainloop's API, never the socket: a message is recorded before it reaches the
 * runner, and a stop, model or effort is Mainloop's to record and apply.
 */
export interface LiveDeps {
  /** Whether the session's actor runs: true, false (asleep), or null when kagent does not say. */
  awake: (sessionId: string) => Promise<boolean | null>;
  /** Mainloop's record of the conversation, which survives the actor sleeping. */
  history: (sessionId: string) => Promise<HistoryMessage[]>;
  /**
   * Records a message under `messageId`, which makes a retry safe. During a turn it steers that turn,
   * as a message sent mid-turn in Claude Code does. Throws with the reason when it is not recorded.
   */
  say: (sessionId: string, messageId: string, text: string) => Promise<SendStatus>;
  /** Stops the running turn, keeping what it already wrote. */
  stop: (sessionId: string) => Promise<void>;
  /** Sets the model or effort for the session's next turns. */
  set: (sessionId: string, what: 'model' | 'effort', value: string) => Promise<void>;
}

type EffortLevel = components['schemas']['EffortLevel'];

const failed = (error: unknown, fallback: string) => new Error(errorDetail(error, fallback));

export const liveDeps: LiveDeps = {
  awake: async (sessionId) => {
    try {
      const { data } = await fetchClient.GET('/sessions/{session_id}/native', {
        params: { path: { session_id: sessionId } }
      });
      const active = data?.preparation_observation?.runtime_association?.current_active;
      return typeof active === 'boolean' ? active : null;
    } catch {
      return null;
    }
  },
  history: async (sessionId) => {
    try {
      const { data } = await fetchClient.GET('/sessions/{session_id}/conversation', {
        params: { path: { session_id: sessionId } }
      });
      return (data?.messages ?? []).map((m) => ({ role: m.role, content: m.content }));
    } catch {
      return [];
    }
  },
  say: async (sessionId, messageId, text) => {
    const { data, error } = await fetchClient.POST('/sessions/{session_id}/message', {
      params: { path: { session_id: sessionId } },
      body: { message: text, message_id: messageId, steer: true }
    });
    if (!data) throw failed(error, 'the message was not recorded');
    return data.status;
  },
  stop: async (sessionId) => {
    const { data, error } = await fetchClient.POST('/sessions/{session_id}/stop-turn', {
      params: { path: { session_id: sessionId } }
    });
    if (!data) throw failed(error, 'the turn was not stopped');
  },
  set: async (sessionId, what, value) => {
    const path = { params: { path: { session_id: sessionId } } };
    const { data, error } =
      what === 'model'
        ? await fetchClient.POST('/sessions/{session_id}/model', {
            ...path,
            body: { model: value }
          })
        : await fetchClient.POST('/sessions/{session_id}/effort', {
            ...path,
            body: { effort: value as EffortLevel }
          });
    if (!data) throw failed(error, `the ${what} was not set`);
  }
};

/** Attempts at recording a message, the same message id each time, before the view says it failed. */
const SAY_ATTEMPTS = 3;

/**
 * The WebSocket URL of a session's live channel. A WebSocket upgrade has no place in the generated
 * fetch client, so this is the one path built by hand; it sits under the same API base, and the
 * browser sends the owner's cookie with it. `wake` lets the attach wake a sleeping session; only an
 * owner action asks for it.
 */
export function liveUrl(
  sessionId: string,
  base: string = window.location.href,
  wake = false
): string {
  const u = new URL(`${API_URL}/sessions/${encodeURIComponent(sessionId)}/live`, base);
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
  if (wake) u.searchParams.set('wake', '1');
  return u.toString();
}

export interface LiveSession {
  state: LiveState;
  conn: Conn;
  /**
   * Sends a message through Mainloop, shown at once. Asleep, it also wakes the session. Retries
   * reuse the message id, so the message is recorded once.
   */
  say: (text: string) => void;
  /** Stops the running turn through Mainloop, as Esc does in Claude Code. */
  interrupt: () => void;
  /** Sets the model or effort through Mainloop, from the next turn. */
  set: (what: 'model' | 'effort', value: string) => void;
  /**
   * Sends a runner control over the socket: only the permission mode, the one control that is not
   * input. It needs an open socket: false, with a note in the log, when there is none.
   */
  send: (cmd: Record<string, unknown>) => boolean;
  /**
   * Answers a permission prompt through `via` (Mainloop's request API), showing the answer at once
   * and taking it back with a note if `via` fails.
   */
  answer: (permId: string, choice: number, via: () => Promise<void>) => void;
  /** Adds a line to the transcript. */
  note: (text: string) => void;
  /** Attaches now, waking the session if it sleeps: an owner action. */
  retry: () => void;
  /** The owner is typing: an attached view is not idle. */
  touch: () => void;
}

/**
 * Attaches to the session's runner and re-attaches after a drop, but never wakes a sleeping session
 * on its own: an attach goes through the Substrate router, which wakes the actor, so a view that
 * reconnected whenever the actor slept would keep it awake for as long as the page is open. Before
 * every attach it did not ask for, it reads whether kagent reports the actor asleep, and the relay
 * refuses a sleeping session without `wake=1` as well.
 *
 * Every attach starts with `hello`, whose backlog rebuilds the live part of the view, so a reconnect
 * never replays or doubles an event. The backlog lives in the runner process and starts empty after a
 * sleep, so the view is seeded from Mainloop's record of the conversation, above the backlog.
 */
export function useLiveSession(sessionId: string, deps: LiveDeps = liveDeps): LiveSession {
  const model = useRef(new LiveModel());
  const ws = useRef<WebSocket | null>(null);
  const [state, setState] = useState<LiveState>(initialState);
  const [conn, setConn] = useState<Conn>('connecting');
  const frame = useRef(0);
  const wakeNow = useRef<() => void>(() => {});
  const asleepNow = useRef(false);
  const bump = useRef<() => void>(() => {});

  const flush = useCallback(() => {
    if (frame.current) return;
    const run = () => {
      frame.current = 0;
      setState({ ...model.current.state });
    };
    frame.current =
      typeof requestAnimationFrame === 'function' && !document.hidden
        ? requestAnimationFrame(run)
        : (setTimeout(run, 16) as unknown as number);
  }, []);

  useEffect(() => {
    let closed = false;
    let backoff = LIVE_TIMING.backoffMs;
    let failures = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let poll: ReturnType<typeof setTimeout> | undefined;
    let idle: ReturnType<typeof setTimeout> | undefined;
    // The owner asked to wake the session; it holds until a runner says hello.
    let wanted = false;
    // Whether an awake read attaches. A view that detached itself waits until kagent reports the
    // actor asleep first, or it would attach again before the actor had a chance to sleep.
    let armed = true;
    // The socket being closed is one the view detached on purpose.
    let detaching: WebSocket | null = null;
    model.current = new LiveModel();

    const loadHistory = () => {
      void deps.history(sessionId).then((messages) => {
        if (closed) return;
        model.current.setHistory(messages);
        flush();
      });
    };

    const sleep = (paused = false) => {
      asleepNow.current = true;
      armed = !paused;
      failures = 0;
      backoff = LIVE_TIMING.backoffMs;
      setConn(paused ? 'paused' : 'asleep');
      loadHistory();
      watch();
    };

    // Asleep or paused, the view only reads; it attaches when something else woke the session.
    const watch = () => {
      clearTimeout(poll);
      poll = setTimeout(() => {
        if (closed || !asleepNow.current) return;
        void deps.awake(sessionId).then((awake) => {
          if (closed || !asleepNow.current) return;
          if (awake === false && !armed) {
            armed = true;
            setConn('asleep');
          }
          if (awake === true && armed) connect(false);
          else watch();
        });
      }, LIVE_TIMING.asleepPollMs);
    };

    // An attached view holds the runner busy; let go when nothing needs it.
    const quiet = () =>
      model.current.state.status === 'idle' &&
      !Object.values(model.current.state.perms).some((p) => p.chosen == null) &&
      model.current.unechoed === 0;
    const arm = () => {
      clearTimeout(idle);
      const sock = ws.current;
      if (closed || !sock || sock.readyState !== WebSocket.OPEN) return;
      idle = setTimeout(
        () => {
          if (closed || ws.current !== sock) return;
          if (!quiet()) {
            arm();
            return;
          }
          detaching = sock;
          sock.close(1000, 'idle');
        },
        document.hidden ? LIVE_TIMING.hiddenDetachMs : LIVE_TIMING.idleDetachMs
      );
    };
    bump.current = arm;

    const connect = (wake: boolean) => {
      if (closed) return;
      clearTimeout(timer);
      clearTimeout(poll);
      asleepNow.current = false;
      armed = true;
      if (ws.current && ws.current.readyState <= WebSocket.OPEN) return;
      if (failures < UNAVAILABLE_AFTER)
        setConn(wake ? 'waking' : failures ? 'reconnecting' : 'connecting');
      let attached = false;
      const sock = new WebSocket(liveUrl(sessionId, window.location.href, wake), LIVE_SUBPROTOCOL);
      ws.current = sock;
      sock.onmessage = (ev) => {
        let f: Frame;
        try {
          f = JSON.parse(String(ev.data)) as Frame;
        } catch {
          return;
        }
        if (!f || typeof f.k !== 'string') return;
        if (f.k === 'hello') {
          attached = true;
          wanted = false;
          backoff = LIVE_TIMING.backoffMs;
          failures = 0;
          setConn('attached');
          model.current.hello(f);
          loadHistory();
          arm();
        } else {
          arm();
          model.current.apply(f);
          if (f.k === 'perm' && document.hidden && 'vibrate' in navigator) navigator.vibrate(80);
        }
        flush();
      };
      sock.onclose = () => {
        if (ws.current === sock) ws.current = null;
        if (closed) return;
        clearTimeout(idle);
        if (detaching === sock) {
          detaching = null;
          sleep(true);
          return;
        }
        failures += 1;
        if (failures >= UNAVAILABLE_AFTER) setConn('unavailable');
        else if (!wanted) setConn('reconnecting');
        const wait = Math.max(backoff, attached ? LIVE_TIMING.settleMs : 0);
        backoff = Math.min(backoff * 2, LIVE_TIMING.backoffMs * 16);
        timer = setTimeout(attempt, wait);
      };
    };

    // An attach the view decides on: if kagent reports the actor asleep, it stays asleep unless the
    // owner asked to wake it.
    const attempt = () => {
      if (closed) return;
      // A message the owner sent that has not shown up yet is an owner action: it wakes the session.
      if (model.current.unechoed) wanted = true;
      if (wanted) {
        connect(true);
        return;
      }
      void deps.awake(sessionId).then((awake) => {
        if (closed || wanted) {
          if (wanted) connect(true);
          return;
        }
        if (awake === false) sleep();
        else connect(false);
      });
    };

    // A phone that slept drops the socket; coming back attaches at once instead of after the backoff,
    // still only if the session is awake.
    const onVisible = () => {
      if (closed) return;
      arm();
      if (document.hidden) return;
      if (asleepNow.current) {
        void deps.awake(sessionId).then((awake) => {
          if (!closed && asleepNow.current && awake === true) connect(false);
        });
        return;
      }
      if (!ws.current || ws.current.readyState > WebSocket.OPEN) {
        clearTimeout(timer);
        backoff = LIVE_TIMING.backoffMs;
        attempt();
      }
    };

    wakeNow.current = () => {
      if (closed || (ws.current && ws.current.readyState <= WebSocket.OPEN)) return;
      wanted = true;
      failures = 0;
      backoff = LIVE_TIMING.backoffMs;
      connect(true);
    };

    loadHistory();
    attempt();
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      closed = true;
      clearTimeout(timer);
      clearTimeout(poll);
      clearTimeout(idle);
      document.removeEventListener('visibilitychange', onVisible);
      ws.current?.close();
      ws.current = null;
    };
  }, [sessionId, flush, deps]);

  const send = useCallback(
    (cmd: Record<string, unknown>) => {
      const sock = ws.current;
      if (sock && sock.readyState === WebSocket.OPEN) {
        sock.send(JSON.stringify(cmd));
        bump.current();
        return true;
      }
      model.current.note(
        asleepNow.current
          ? 'The session is asleep; send a message or wake it first.'
          : 'Not connected; not sent.'
      );
      flush();
      return false;
    },
    [flush]
  );

  const answer = useCallback(
    (permId: string, choice: number, via: () => Promise<void>) => {
      if (!model.current.choose(permId, choice)) return;
      bump.current();
      flush();
      via().catch((err: unknown) => {
        model.current.unchoose(permId);
        model.current.note(`Not answered: ${err instanceof Error ? err.message : String(err)}`);
        flush();
      });
    },
    [flush]
  );

  const note = useCallback(
    (text: string) => {
      model.current.note(text);
      flush();
    },
    [flush]
  );

  const say = useCallback(
    (text: string) => {
      const mid = crypto.randomUUID();
      model.current.sending(mid, text);
      if (asleepNow.current) {
        model.current.note('Waking the session; your message is recorded and goes when it is up.');
        wakeNow.current();
      }
      bump.current();
      flush();
      const attempt = async (n: number): Promise<void> => {
        try {
          const status = await deps.say(sessionId, mid, text);
          if (status !== 'ok') model.current.recorded(mid, status === 'steered');
        } catch (err) {
          if (n + 1 < SAY_ATTEMPTS) {
            await new Promise((r) => setTimeout(r, LIVE_TIMING.backoffMs * 2 ** n));
            return attempt(n + 1);
          }
          model.current.notSent(mid, err instanceof Error ? err.message : String(err));
        }
        flush();
      };
      void attempt(0);
    },
    [sessionId, deps, flush]
  );

  const interrupt = useCallback(() => {
    void deps.stop(sessionId).catch((err: unknown) => {
      model.current.note(`Not stopped: ${err instanceof Error ? err.message : String(err)}`);
      flush();
    });
  }, [sessionId, deps, flush]);

  const set = useCallback(
    (what: 'model' | 'effort', value: string) => {
      void deps
        .set(sessionId, what, value)
        .then(
          () => model.current.control(what, value),
          (err: unknown) =>
            model.current.note(`Not set: ${err instanceof Error ? err.message : String(err)}`)
        )
        .finally(flush);
    },
    [sessionId, deps, flush]
  );

  const retry = useCallback(() => wakeNow.current(), []);
  const touch = useCallback(() => bump.current(), []);

  return { state, conn, say, interrupt, set, send, answer, note, retry, touch };
}
