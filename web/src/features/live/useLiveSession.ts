import { useCallback, useEffect, useRef, useState } from 'react';
import { API_URL } from '@/api/config';
import { fetchClient } from '@/api/client';
import { initialState, LiveModel, type Frame, type HistoryMessage, type LiveState } from './store';

export const LIVE_SUBPROTOCOL = 'kagent.sdk-runner.v1';

/**
 * `asleep`: kagent reports the session's actor asleep, and nothing attaches until the owner acts
 * (sends a message or presses wake) or something else wakes it (an A2A turn, an answered request).
 * `waking`: the owner asked, and the attach that wakes it is under way.
 */
export type Conn = 'connecting' | 'attached' | 'reconnecting' | 'asleep' | 'waking' | 'unavailable';

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
 * session. Tests shorten them.
 */
export const LIVE_TIMING = { backoffMs: 500, settleMs: 1500, asleepPollMs: 10_000 };

/** What the view reads from Mainloop, outside the live channel. Neither read wakes the session. */
export interface LiveDeps {
  /** Whether the session's actor runs: true, false (asleep), or null when kagent does not say. */
  awake: (sessionId: string) => Promise<boolean | null>;
  /** Mainloop's record of the conversation, which survives the actor sleeping. */
  history: (sessionId: string) => Promise<HistoryMessage[]>;
}

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
  }
};

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
   * Sends a client command. A message sent while the session is asleep or reconnecting waits and
   * goes once the runner says hello; asleep, it wakes the session. Anything else needs an open
   * socket: false, with a note in the log, when there is none.
   */
  send: (cmd: Record<string, unknown>) => boolean;
  answer: (permId: string, choice: number) => void;
  /** Attaches now, waking the session if it sleeps: an owner action. */
  retry: () => void;
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
  const pending = useRef<Record<string, unknown>[]>([]);

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
    // The owner asked to wake the session; it holds until a runner says hello.
    let wanted = false;
    model.current = new LiveModel();
    pending.current = [];

    const loadHistory = () => {
      void deps.history(sessionId).then((messages) => {
        if (closed) return;
        model.current.setHistory(messages);
        flush();
      });
    };

    const sleep = () => {
      asleepNow.current = true;
      failures = 0;
      backoff = LIVE_TIMING.backoffMs;
      setConn('asleep');
      loadHistory();
      watch();
    };

    // Asleep, the view only reads; it attaches when something else woke the session.
    const watch = () => {
      clearTimeout(poll);
      poll = setTimeout(() => {
        if (closed || !asleepNow.current) return;
        void deps.awake(sessionId).then((awake) => {
          if (closed || !asleepNow.current) return;
          if (awake === true) connect(false);
          else watch();
        });
      }, LIVE_TIMING.asleepPollMs);
    };

    const connect = (wake: boolean) => {
      if (closed) return;
      clearTimeout(timer);
      clearTimeout(poll);
      asleepNow.current = false;
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
          for (const cmd of pending.current.splice(0)) sock.send(JSON.stringify(cmd));
        } else {
          model.current.apply(f);
          if (f.k === 'perm' && document.hidden && 'vibrate' in navigator) navigator.vibrate(80);
        }
        flush();
      };
      sock.onclose = () => {
        if (ws.current === sock) ws.current = null;
        if (closed) return;
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
      // A message the owner already sent is an owner action: it wakes the session.
      if (pending.current.length) wanted = true;
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
      if (document.hidden || closed) return;
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
        return true;
      }
      if (cmd.k === 'user') {
        pending.current.push(cmd);
        if (asleepNow.current) {
          model.current.note('Waking the session; your message goes when it is up.');
          wakeNow.current();
        } else {
          model.current.note('Reconnecting; your message goes when the session is back.');
        }
        flush();
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
    (permId: string, choice: number) => {
      if (!model.current.choose(permId, choice)) return;
      send({
        k: 'perm',
        id: permId,
        behavior: choice === 2 ? 'deny' : 'allow',
        always: choice === 1
      });
      flush();
    },
    [send, flush]
  );

  const retry = useCallback(() => wakeNow.current(), []);

  return { state, conn, send, answer, retry };
}
