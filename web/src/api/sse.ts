import { API_URL } from './config';

export interface SseMessage {
  event: string;
  data: string;
  id: string;
}

/**
 * Incremental parser for the text/event-stream format. Feed it decoded chunks; it calls
 * `onMessage` per dispatched event. `lastEventId` persists across events, as the spec requires.
 */
export function createSseParser(onMessage: (message: SseMessage) => void) {
  let buffer = '';
  let event = '';
  let data: string[] = [];
  let lastEventId = '';
  let skipLf = false;

  function dispatch() {
    if (data.length)
      onMessage({ event: event || 'message', data: data.join('\n'), id: lastEventId });
    event = '';
    data = [];
  }

  function line(text: string) {
    if (text === '') return dispatch();
    if (text.startsWith(':')) return;
    const colon = text.indexOf(':');
    const field = colon === -1 ? text : text.slice(0, colon);
    let value = colon === -1 ? '' : text.slice(colon + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    if (field === 'event') event = value;
    else if (field === 'data') data.push(value);
    else if (field === 'id' && !value.includes('\0')) lastEventId = value;
  }

  return {
    push(chunk: string) {
      if (skipLf && chunk.startsWith('\n')) chunk = chunk.slice(1);
      skipLf = false;
      buffer += chunk;
      let start = 0;
      for (let i = 0; i < buffer.length; i++) {
        const ch = buffer[i];
        if (ch !== '\n' && ch !== '\r') continue;
        line(buffer.slice(start, i));
        if (ch === '\r') {
          if (buffer[i + 1] === '\n') i++;
          else if (i + 1 === buffer.length) skipLf = true;
        }
        start = i + 1;
      }
      buffer = buffer.slice(start);
    },
    get lastEventId() {
      return lastEventId;
    }
  };
}

export type SseStatus = 'connecting' | 'open' | 'closed';

export interface SseClientOptions {
  url?: string;
  onMessage: (message: SseMessage) => void;
  onStatus?: (status: SseStatus) => void;
  fetch?: typeof fetch;
  /** Delay before reconnect attempt n (0-based), in ms. */
  backoff?: (attempt: number) => number;
  /** Waits `ms`; resolves early when `signal` aborts or the network or tab comes back. */
  sleep?: (ms: number, signal: AbortSignal) => Promise<void>;
  /** Reconnect when no bytes arrive for this long (default 75 s; the server heartbeats at 30 s). */
  idleTimeoutMs?: number;
}

export const SSE_IDLE_TIMEOUT_MS = 75_000;

const defaultBackoff = (attempt: number) =>
  Math.min(30_000, 1000 * 2 ** attempt) * (0.75 + Math.random() * 0.5);

/**
 * A backoff wait that ends early on `online` and when the tab becomes visible: a user who just
 * regained the network or came back to the tab should not wait out a 30 s delay.
 */
const defaultSleep = (ms: number, signal: AbortSignal) =>
  new Promise<void>((resolve) => {
    const onVisible = () => {
      if (document.visibilityState === 'visible') finish();
    };
    const finish = () => {
      clearTimeout(timer);
      signal.removeEventListener('abort', finish);
      window.removeEventListener('online', finish);
      document.removeEventListener('visibilitychange', onVisible);
      resolve();
    };
    const timer = setTimeout(finish, ms);
    if (signal.aborted) return finish();
    signal.addEventListener('abort', finish);
    window.addEventListener('online', finish);
    document.addEventListener('visibilitychange', onVisible);
  });

/**
 * SSE over fetch, not EventSource: a fresh connection can send `Last-Event-ID` itself, so a
 * reconnect after an error or a server restart replays from the last event seen instead of
 * starting over. Reconnects with jittered exponential backoff until `stop()`.
 *
 * A watchdog drops a connection that has gone silent: a half-open TCP connection (sleeping laptop,
 * dead NAT entry) neither errors nor ends, so without it the stream would hang forever.
 */
export function createSseClient(options: SseClientOptions) {
  const url = options.url ?? `${API_URL}/events`;
  const doFetch = options.fetch ?? ((...args: Parameters<typeof fetch>) => fetch(...args));
  const backoff = options.backoff ?? defaultBackoff;
  const sleep = options.sleep ?? defaultSleep;
  const idleTimeoutMs = options.idleTimeoutMs ?? SSE_IDLE_TIMEOUT_MS;
  let controller: AbortController | undefined;
  let lastEventId = '';

  /** One connection, ending when the stream ends, errors, goes silent, or `signal` aborts. */
  async function connect(signal: AbortSignal): Promise<boolean> {
    const attemptController = new AbortController();
    const abortAttempt = () => attemptController.abort();
    signal.addEventListener('abort', abortAttempt, { once: true });
    let reader: ReadableStreamDefaultReader<string> | undefined;
    let idle: ReturnType<typeof setTimeout> | undefined;
    const arm = () => {
      clearTimeout(idle);
      idle = setTimeout(() => {
        attemptController.abort();
        // A stalled body read does not always reject on abort.
        void reader?.cancel().catch(() => {});
      }, idleTimeoutMs);
    };
    let opened = false;
    try {
      arm();
      const headers: Record<string, string> = { Accept: 'text/event-stream' };
      if (lastEventId) headers['Last-Event-ID'] = lastEventId;
      const response = await doFetch(url, {
        headers,
        signal: attemptController.signal,
        cache: 'no-store'
      });
      if (!response.ok || !response.body) throw new Error(`SSE ${response.status}`);
      options.onStatus?.('open');
      opened = true;
      const parser = createSseParser((message) => {
        if (message.id) lastEventId = message.id;
        options.onMessage(message);
      });
      reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        arm();
        parser.push(value);
      }
    } catch {
      // Falls through to the reconnect delay; an abort ends the loop in run().
    } finally {
      clearTimeout(idle);
      signal.removeEventListener('abort', abortAttempt);
      attemptController.abort();
    }
    return opened;
  }

  async function run(signal: AbortSignal) {
    let attempt = 0;
    while (!signal.aborted) {
      options.onStatus?.('connecting');
      if (await connect(signal)) attempt = 0;
      if (signal.aborted) break;
      options.onStatus?.('connecting');
      await sleep(backoff(attempt++), signal);
    }
    options.onStatus?.('closed');
  }

  return {
    start() {
      if (controller) return;
      controller = new AbortController();
      void run(controller.signal);
    },
    stop() {
      controller?.abort();
      controller = undefined;
    },
    get lastEventId() {
      return lastEventId;
    }
  };
}
