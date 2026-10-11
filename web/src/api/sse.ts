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
  sleep?: (ms: number, signal: AbortSignal) => Promise<void>;
}

const defaultBackoff = (attempt: number) =>
  Math.min(30_000, 1000 * 2 ** attempt) * (0.75 + Math.random() * 0.5);

const defaultSleep = (ms: number, signal: AbortSignal) =>
  new Promise<void>((resolve) => {
    const timer = setTimeout(resolve, ms);
    signal.addEventListener('abort', () => {
      clearTimeout(timer);
      resolve();
    });
  });

/**
 * SSE over fetch, not EventSource: a fresh connection can send `Last-Event-ID` itself, so a
 * reconnect after an error or a server restart replays from the last event seen instead of
 * starting over. Reconnects with jittered exponential backoff until `stop()`.
 */
export function createSseClient(options: SseClientOptions) {
  const url = options.url ?? `${API_URL}/events`;
  const doFetch = options.fetch ?? ((...args: Parameters<typeof fetch>) => fetch(...args));
  const backoff = options.backoff ?? defaultBackoff;
  const sleep = options.sleep ?? defaultSleep;
  let controller: AbortController | undefined;
  let lastEventId = '';

  async function run(signal: AbortSignal) {
    let attempt = 0;
    while (!signal.aborted) {
      options.onStatus?.('connecting');
      try {
        const headers: Record<string, string> = { Accept: 'text/event-stream' };
        if (lastEventId) headers['Last-Event-ID'] = lastEventId;
        const response = await doFetch(url, { headers, signal, cache: 'no-store' });
        if (!response.ok || !response.body) throw new Error(`SSE ${response.status}`);
        options.onStatus?.('open');
        attempt = 0;
        const parser = createSseParser((message) => {
          if (message.id) lastEventId = message.id;
          options.onMessage(message);
        });
        const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          parser.push(value);
        }
      } catch {
        // Falls through to the reconnect delay; an abort ends the loop below.
      }
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
