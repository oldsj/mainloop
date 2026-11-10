import { useCallback, useEffect, useRef, useState } from 'react';
import { API_URL } from '@/api/config';
import { initialState, LiveModel, type Frame, type LiveState } from './store';

export const LIVE_SUBPROTOCOL = 'kagent.sdk-runner.v1';

export type Conn = 'connecting' | 'attached' | 'reconnecting';

/**
 * The WebSocket URL of a session's live channel. A WebSocket upgrade has no place in the generated
 * fetch client, so this is the one path built by hand; it sits under the same API base, and the
 * browser sends the owner's cookie with it.
 */
export function liveUrl(sessionId: string, base: string = window.location.href): string {
  const u = new URL(`${API_URL}/sessions/${encodeURIComponent(sessionId)}/live`, base);
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
  return u.toString();
}

export interface LiveSession {
  state: LiveState;
  conn: Conn;
  /** Sends a client command. False when the socket is not open; a note says so in the log. */
  send: (cmd: Record<string, unknown>) => boolean;
  answer: (permId: string, choice: number) => void;
}

/**
 * Attaches to the session's runner and keeps re-attaching. Every attach starts with `hello`, whose
 * backlog rebuilds the whole view, so a reconnect never replays or doubles an event.
 */
export function useLiveSession(sessionId: string): LiveSession {
  const model = useRef(new LiveModel());
  const ws = useRef<WebSocket | null>(null);
  const [state, setState] = useState<LiveState>(initialState);
  const [conn, setConn] = useState<Conn>('connecting');
  const frame = useRef(0);

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
    let backoff = 500;
    let timer: ReturnType<typeof setTimeout> | undefined;

    const connect = () => {
      if (closed) return;
      const sock = new WebSocket(liveUrl(sessionId), LIVE_SUBPROTOCOL);
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
          backoff = 500;
          setConn('attached');
          model.current.hello(f);
        } else {
          model.current.apply(f);
          if (f.k === 'perm' && document.hidden && 'vibrate' in navigator) navigator.vibrate(80);
        }
        flush();
      };
      sock.onclose = () => {
        if (ws.current === sock) ws.current = null;
        if (closed) return;
        setConn('reconnecting');
        timer = setTimeout(connect, backoff);
        backoff = Math.min(backoff * 2, 8000);
      };
    };

    // A phone that slept drops the socket; coming back reconnects at once instead of after the backoff.
    const onVisible = () => {
      if (document.hidden || closed) return;
      if (!ws.current || ws.current.readyState > WebSocket.OPEN) {
        clearTimeout(timer);
        backoff = 500;
        connect();
      }
    };

    connect();
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      closed = true;
      clearTimeout(timer);
      document.removeEventListener('visibilitychange', onVisible);
      ws.current?.close();
      ws.current = null;
    };
  }, [sessionId, flush]);

  const send = useCallback(
    (cmd: Record<string, unknown>) => {
      const sock = ws.current;
      if (sock && sock.readyState === WebSocket.OPEN) {
        sock.send(JSON.stringify(cmd));
        return true;
      }
      model.current.note('Not connected; not sent.');
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

  return { state, conn, send, answer };
}
